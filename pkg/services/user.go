package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/tgdrive/teldrive/internal/api"
	"github.com/tgdrive/teldrive/internal/auth"
	"github.com/tgdrive/teldrive/internal/cache"
	"github.com/tgdrive/teldrive/internal/category"
	"github.com/tgdrive/teldrive/internal/tgc"
	"github.com/tgdrive/teldrive/internal/tgstorage"
	"github.com/tgdrive/teldrive/internal/utils"
	"github.com/tgdrive/teldrive/pkg/models"
	"gorm.io/datatypes"

	"github.com/gotd/contrib/storage"
	"gorm.io/gorm/clause"
)

const (
	importDefaultPath     = "/root"
	importHistoryBatch    = 100
	importFallbackMime    = "application/octet-stream"
	importFallbackNameFmt = "file_%d"
)

func (a *apiService) UsersAddBots(ctx context.Context, req *api.AddBots) error {
	userID := auth.GetUser(ctx)

	payload := []models.Bot{}
	if len(req.Bots) > 0 {
		for _, token := range req.Bots {
			payload = append(payload, models.Bot{UserId: userID, Token: token})
		}
		if err := a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&payload).Error; err != nil {
			return err
		}
		var channels []int64
		if err := a.db.Model(&models.Channel{}).Where("user_id = ?", userID).Pluck("channel_id", &channels).Error; err != nil {
			return err
		}
		if len(channels) > 0 {
			for _, channel := range channels {
				a.channelManager.AddBotsToChannel(ctx, userID, channel, req.Bots, false)
			}
		}
		a.cache.Delete(ctx, cache.KeyUserBots(userID))
	}
	return nil

}

type channelStat struct {
	ChannelId int64
	FileCount int64
	TotalSize int64
}

func (a *apiService) UsersListChannels(ctx context.Context) ([]api.Channel, error) {

	userId := auth.GetUser(ctx)

	channels := make(map[int64]*api.Channel)

	// Load channels stored in the database (these have been set up for TelDrive storage).
	var dbChannels []models.Channel
	if err := a.db.WithContext(ctx).Where("user_id = ?", userId).Find(&dbChannels).Error; err != nil {
		return nil, &apiError{err: err}
	}
	for _, ch := range dbChannels {
		channels[ch.ChannelId] = &api.Channel{
			ChannelId:   api.NewOptInt64(ch.ChannelId),
			ChannelName: ch.ChannelName,
			Selected:    api.NewOptBool(ch.Selected),
		}
	}

	// Supplement with channels discovered from Telegram peer storage.
	peerStorage := tgstorage.NewPeerStorage(a.db, cache.KeyPeer(userId))

	iter, err := peerStorage.Iterate(ctx)
	if err != nil {
		return []api.Channel{}, nil
	}
	defer iter.Close()
	for iter.Next(ctx) {
		peer := iter.Value()
		if peer.Channel != nil && peer.Channel.AdminRights.AddAdmins {
			if _, exists := channels[peer.Channel.ID]; !exists {
				channels[peer.Channel.ID] = &api.Channel{
					ChannelId:   api.NewOptInt64(peer.Channel.ID),
					ChannelName: peer.Channel.Title,
					Selected:    api.NewOptBool(false),
				}
			}
		}
	}

	// Include channels that have files stored but are not yet in the map
	// (e.g. channels discovered from file history that are no longer in peer storage),
	// and populate per-channel file statistics in a single query.
	var stats []channelStat
	if err := a.db.WithContext(ctx).Model(&models.File{}).
		Select("channel_id, count(*) as file_count, COALESCE(SUM(size), 0) as total_size").
		Where("user_id = ? AND status = 'active' AND type = 'file' AND channel_id IS NOT NULL", userId).
		Group("channel_id").
		Scan(&stats).Error; err == nil {
		for _, stat := range stats {
			if _, exists := channels[stat.ChannelId]; !exists {
				channels[stat.ChannelId] = &api.Channel{
					ChannelId:   api.NewOptInt64(stat.ChannelId),
					ChannelName: fmt.Sprintf("channel_%d", stat.ChannelId),
					Selected:    api.NewOptBool(false),
				}
			}
			if ch, exists := channels[stat.ChannelId]; exists {
				ch.FileCount = api.NewOptInt64(stat.FileCount)
				ch.TotalSize = api.NewOptInt64(stat.TotalSize)
			}
		}
	}

	res := make([]api.Channel, 0, len(channels))
	for _, channel := range channels {
		res = append(res, *channel)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].ChannelName < res[j].ChannelName
	})
	return res, nil
}

func (a *apiService) UsersCreateChannel(ctx context.Context, req *api.Channel) error {
	userID := auth.GetUser(ctx)
	_, err := a.channelManager.CreateNewChannel(ctx, req.ChannelName, userID, false)
	if err != nil {
		return &apiError{err: err}
	}
	return nil
}

func (a *apiService) UsersDeleteChannel(ctx context.Context, params api.UsersDeleteChannelParams) error {
	userId := auth.GetUser(ctx)
	client, _ := tgc.AuthClient(ctx, &a.cnf.TG, auth.GetJWTUser(ctx).TgSession, a.newMiddlewares(ctx, 5)...)
	channelId, _ := strconv.ParseInt(params.ID, 10, 64)
	peerStorage := tgstorage.NewPeerStorage(a.db, cache.KeyPeer(userId))
	var (
		channel *tg.Channel
		err     error
	)
	err = client.Run(ctx, func(ctx context.Context) error {
		channel, err = tgc.GetChannelFull(ctx, client.API(), channelId)
		if err != nil {
			return err
		}
		_, err = client.API().ChannelsDeleteChannel(ctx, channel.AsInput())
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return &apiError{err: err}
	}
	a.db.Where("channel_id = ?", channelId).Delete(&models.Channel{})
	peer := storage.Peer{}
	peer.FromChat(channel)
	peerStorage.Delete(ctx, storage.KeyFromPeer(peer))
	return nil
}

func (a *apiService) UsersSyncChannels(ctx context.Context) error {
	userId := auth.GetUser(ctx)
	peerStorage := tgstorage.NewPeerStorage(a.db, cache.KeyPeer(userId))
	err := peerStorage.Purge(ctx)
	if err != nil {
		return &apiError{err: err}
	}
	collector := storage.CollectPeers(peerStorage)
	client, err := tgc.AuthClient(ctx, &a.cnf.TG, auth.GetJWTUser(ctx).TgSession, a.newMiddlewares(ctx, 5)...)
	if err != nil {
		return &apiError{err: err}
	}
	err = client.Run(ctx, func(ctx context.Context) error {
		return collector.Dialogs(ctx, query.GetDialogs(client.API()).Iter())
	})
	if err != nil {
		return &apiError{err: err}
	}
	return nil
}

func (a *apiService) UsersListSessions(ctx context.Context) ([]api.UserSession, error) {
	userId := auth.GetUser(ctx)
	return cache.Fetch(ctx, a.cache, cache.KeyUserSessions(userId), 0, func() ([]api.UserSession, error) {
		userSession := auth.GetJWTUser(ctx).TgSession
		client, _ := tgc.AuthClient(ctx, &a.cnf.TG, userSession, a.newMiddlewares(ctx, 5)...)
		var (
			auth *tg.AccountAuthorizations
			err  error
		)
		err = client.Run(ctx, func(ctx context.Context) error {
			auth, err = client.API().AccountGetAuthorizations(ctx)
			if err != nil {
				return err
			}
			return nil
		})

		if err != nil && !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return nil, err
		}

		dbSessions := []models.Session{}

		if err = a.db.Where("user_id = ?", userId).Order("created_at DESC").Find(&dbSessions).Error; err != nil {
			return nil, err
		}

		sessionsOut := []api.UserSession{}

		for _, session := range dbSessions {

			s := api.UserSession{Hash: session.Hash,
				CreatedAt: session.CreatedAt.UTC(),
				Current:   session.Session == userSession}

			if auth != nil {
				for _, a := range auth.Authorizations {
					if session.SessionDate == a.DateCreated {
						s.AppName = api.NewOptString(strings.Trim(strings.ReplaceAll(a.AppName, "Telegram", ""), " "))
						s.Location = api.NewOptString(a.Country)
						s.OfficialApp = api.NewOptBool(a.OfficialApp)
						s.Valid = true
						break
					}
				}
			}

			sessionsOut = append(sessionsOut, s)
		}

		return sessionsOut, nil

	})

}

func (a *apiService) UsersProfileImage(ctx context.Context, params api.UsersProfileImageParams) (*api.UsersProfileImageOKHeaders, error) {

	client, err := tgc.AuthClient(ctx, &a.cnf.TG, auth.GetJWTUser(ctx).TgSession, a.newMiddlewares(ctx, 5)...)

	if err != nil {
		return nil, &apiError{err: err}
	}

	res := &api.UsersProfileImageOKHeaders{}

	err = tgc.RunWithAuth(ctx, client, "", func(ctx context.Context) error {
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		peer := self.AsInputPeer()
		if self.Photo == nil {
			return nil
		}
		photo, ok := self.Photo.AsNotEmpty()
		if !ok {
			return errors.New("profile not found")
		}
		photo.GetPersonal()
		location := &tg.InputPeerPhotoFileLocation{Big: false, Peer: peer, PhotoID: photo.PhotoID}
		buff, err := tgc.GetMediaContent(ctx, client.API(), location)
		if err != nil {
			return err
		}
		content := buff.Bytes()
		res.SetCacheControl("public, max-age=86400, must-revalidate")
		res.SetContentLength(int64(len(content)))
		res.SetEtag(fmt.Sprintf("\"%v\"", photo.PhotoID))
		res.SetContentDisposition(fmt.Sprintf("inline; filename=\"%s\"", "profile.jpeg"))
		res.Response = api.UsersProfileImageOK{Data: bytes.NewReader(content)}
		return nil
	})
	if err != nil {
		return nil, &apiError{err: err}
	}
	return res, nil
}

func (a *apiService) UsersRemoveBots(ctx context.Context) error {
	userId := auth.GetUser(ctx)

	if err := a.db.Where("user_id = ?", userId).Delete(&models.Bot{}).Error; err != nil {
		return &apiError{err: err}
	}
	a.cache.Delete(ctx, cache.KeyUserBots(userId))

	return nil
}

func (a *apiService) UsersRemoveSession(ctx context.Context, params api.UsersRemoveSessionParams) error {
	userId := auth.GetUser(ctx)

	session := &models.Session{}

	if err := a.db.Where("user_id = ?", userId).Where("hash = ?", params.ID).First(session).Error; err != nil {
		return &apiError{err: err}
	}

	client, _ := tgc.AuthClient(ctx, &a.cnf.TG, session.Session, a.newMiddlewares(ctx, 5)...)

	client.Run(ctx, func(ctx context.Context) error {
		_, err := client.API().AuthLogOut(ctx)
		if err != nil {
			return err
		}
		return nil
	})

	a.db.Where("user_id = ?", userId).Where("hash = ?", session.Hash).Delete(&models.Session{})
	a.cache.Delete(ctx, cache.KeyUserSessions(userId))

	return nil
}

func (a *apiService) UsersStats(ctx context.Context) (*api.UserConfig, error) {
	userId := auth.GetUser(ctx)
	var (
		channelId int64
		err       error
	)

	channelId, err = a.channelManager.CurrentChannel(ctx, userId)
	if err != nil {
		channelId = 0
	}

	tokens, err := a.channelManager.BotTokens(ctx, userId)

	if err != nil {
		tokens = []string{}
	}
	return &api.UserConfig{Bots: tokens, ChannelId: channelId}, nil
}

func (a *apiService) UsersUpdateChannel(ctx context.Context, req *api.ChannelUpdate) error {
	userId := auth.GetUser(ctx)

	channel := &models.Channel{UserId: userId, Selected: true}

	if req.ChannelId.Value != 0 {
		channel.ChannelId = req.ChannelId.Value
	}
	if req.ChannelName.Value != "" {
		channel.ChannelName = req.ChannelName.Value
	}

	if err := a.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "channel_id"}},
		DoUpdates: clause.Assignments(map[string]any{"selected": true}),
	}).Create(channel).Error; err != nil {
		return &apiError{err: errors.New("failed to update channel")}
	}
	a.db.Model(&models.Channel{}).Where("channel_id != ?", channel.ChannelId).
		Where("user_id = ?", userId).Update("selected", false)

	a.cache.Set(ctx, cache.KeyUserChannel(userId), channel.ChannelId, 0)
	return nil
}

func (a *apiService) UsersImportChannel(ctx context.Context, req *api.ChannelImport, params api.UsersImportChannelParams) (*api.ImportResult, error) {
	userId := auth.GetUser(ctx)

	channelId, err := strconv.ParseInt(params.ID, 10, 64)
	if err != nil {
		return nil, &apiError{err: errors.New("invalid channel id"), code: 400}
	}

	// Collect message IDs already tracked in TelDrive for this channel so we
	// can skip them during the import walk.
	type partIDRow struct {
		PartID int `gorm:"column:part_id"`
	}
	var existingRows []partIDRow
	if err := a.db.WithContext(ctx).Raw(`
		SELECT (part->>'id')::int AS part_id
		FROM teldrive.files, jsonb_array_elements(parts) AS part
		WHERE channel_id = ? AND user_id = ? AND type = 'file' AND status = 'active'
	`, channelId, userId).Scan(&existingRows).Error; err != nil {
		return nil, &apiError{err: err}
	}
	existingPartIDs := make(map[int]struct{}, len(existingRows))
	for _, r := range existingRows {
		existingPartIDs[r.PartID] = struct{}{}
	}

	// Resolve the destination directory, creating it if necessary.
	destPath := strings.TrimSpace(req.GetPath().Or(importDefaultPath))
	if destPath == "" {
		destPath = importDefaultPath
	}
	var destRes []models.File
	if err := a.db.WithContext(ctx).Raw("SELECT * FROM teldrive.create_directories(?, ?)", userId, destPath).
		Scan(&destRes).Error; err != nil {
		return nil, &apiError{err: err}
	}
	var parentId *string
	if len(destRes) > 0 {
		parentId = &destRes[0].ID
	}

	// Open a Telegram client for the authenticated user.
	client, err := tgc.AuthClient(ctx, &a.cnf.TG, auth.GetJWTUser(ctx).TgSession, a.newMiddlewares(ctx, 5)...)
	if err != nil {
		return nil, &apiError{err: err}
	}

	imported := 0
	total := 0

	err = tgc.RunWithAuth(ctx, client, "", func(ctx context.Context) error {
		channel, err := tgc.GetChannelFull(ctx, client.API(), channelId)
		if err != nil {
			return err
		}
		inputPeer := channel.AsInputPeer()

		iter := query.NewQuery(client.API()).Messages().GetHistory(inputPeer).BatchSize(importHistoryBatch).Iter()
		for iter.Next(ctx) {
			elem := iter.Value()
			msg, ok := elem.Msg.(*tg.Message)
			if !ok {
				continue
			}
			media, ok := msg.Media.(*tg.MessageMediaDocument)
			if !ok {
				continue
			}
			document, ok := media.Document.(*tg.Document)
			if !ok {
				continue
			}
			total++

			// Skip messages already tracked in TelDrive.
			if _, exists := existingPartIDs[msg.ID]; exists {
				continue
			}

			// Extract filename from document attributes.
			fileName := fmt.Sprintf(importFallbackNameFmt, msg.ID)
			for _, attr := range document.Attributes {
				if fnAttr, ok := attr.(*tg.DocumentAttributeFilename); ok {
					fileName = fnAttr.FileName
					break
				}
			}

			mimeType := document.MimeType
			if mimeType == "" {
				mimeType = importFallbackMime
			}
			cat := string(category.GetCategory(fileName))
			size := document.Size
			parts := datatypes.NewJSONSlice([]api.Part{{ID: msg.ID}})
			now := time.Now().UTC()

			dbFile := models.File{
				Name:      fileName,
				Type:      "file",
				MimeType:  mimeType,
				Size:      &size,
				Category:  &cat,
				UserId:    userId,
				Status:    "active",
				ParentId:  parentId,
				ChannelId: &channelId,
				Encrypted: utils.Ptr(false),
				Parts:     utils.Ptr(parts),
				UpdatedAt: &now,
			}

			// Use the same raw-SQL upsert as FilesCreate to honour the partial
			// unique index on (name, parent_id, user_id) WHERE status='active'.
			var inserted models.File
			if err := a.db.WithContext(ctx).Raw(`
				INSERT INTO teldrive.files (
					name, parent_id, user_id, mime_type, category, parts,
					size, type, encrypted, updated_at, channel_id, status
				)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (name, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), user_id)
				WHERE status = 'active'
				DO NOTHING
				RETURNING *
			`,
				dbFile.Name, dbFile.ParentId, dbFile.UserId, dbFile.MimeType,
				dbFile.Category, dbFile.Parts, dbFile.Size, dbFile.Type,
				dbFile.Encrypted, dbFile.UpdatedAt, dbFile.ChannelId, dbFile.Status,
			).Scan(&inserted).Error; err != nil {
				return err
			}
			if inserted.ID != "" {
				imported++
				// Track newly imported part ID so a duplicate message later in
				// the same scan is also skipped.
				existingPartIDs[msg.ID] = struct{}{}
			}
		}
		return iter.Err()
	})

	if err != nil {
		return nil, &apiError{err: err}
	}

	return &api.ImportResult{Imported: imported, Total: total}, nil
}
