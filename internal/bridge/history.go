package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
)

func (b *Bridge) QueueHistory(conversation, folder string, restart bool) (model.HistoryJob, error) {
	job := model.HistoryJob{Kind: "messages", ConversationID: conversation, ID: "messages:" + conversation}
	if conversation != "" {
		if current, err := b.Store.EntityCurrent("conversation", conversation); err != nil {
			return job, err
		} else if !current {
			return job, ErrInvalid
		}
		if folder != "" || len(conversation) > 256 {
			return job, ErrInvalid
		}
		if _, err := b.Store.Record("conversation", conversation); err != nil {
			return job, err
		}
	} else {
		if folder != "inbox" && folder != "archive" && folder != "spam" {
			return job, ErrInvalid
		}
		job = model.HistoryJob{Kind: "conversations", Folder: folder, ID: "conversations:" + folder}
	}
	job, err := b.Store.QueueHistory(job, restart)
	if err == nil {
		b.Hub.Notify()
		wake(b.historyWake)
	}
	return job, err
}
func (b *Bridge) historyLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.historyWake:
		}
		if ctx.Err() != nil {
			return
		}
		if b.Status().State != "connected" {
			continue
		}
		if err := b.historyOne(ctx); err != nil {
			b.storageFailure(err)
			return
		}
	}
}
func (b *Bridge) historyOne(ctx context.Context) error {
	p := b.getProvider()
	if p == nil {
		return nil
	}
	raw, err := b.Store.Latest("history")
	if err != nil {
		return err
	}
	var jobs []model.HistoryJob
	for _, data := range raw {
		var job model.HistoryJob
		if err = json.Unmarshal(data, &job); err != nil {
			return err
		}
		if job.State == "queued" && !job.RetryAt.After(time.Now()) {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Updated.Before(jobs[j].Updated) })
	if len(jobs) == 0 {
		return nil
	}
	job := jobs[0]
	cursor, err := b.Store.HistoryCursor(job.ID)
	if err != nil {
		return err
	}
	mark, err := b.Store.HistoryWatermark()
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var snapshots []provider.Snapshot
	var next []byte
	if job.Kind == "messages" {
		snapshots, next, err = p.MessagePage(callCtx, job.ConversationID, cursor)
	} else {
		snapshots, next, err = p.ConversationPage(callCtx, job.Folder, cursor)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		if errors.Is(err, provider.ErrUnsupportedCursor) || errors.Is(err, provider.ErrInvalidCursor) || errors.Is(err, provider.ErrInvalidFolder) {
			err = b.Store.FailHistory(job.ID, job.Generation, "Provider history cursor is unsupported or invalid; import stopped")
		} else {
			err = b.Store.CheckpointHistory(job.ID, job.Generation, cursor, cursor, 0, "Phone history unavailable; will retry")
		}
		b.Hub.Notify()
		return err
	}
	for _, snap := range snapshots {
		if err = b.apply(snap, mark); err != nil {
			return err
		}
		if job.Kind == "conversations" {
			if _, err = b.QueueHistory(snap.Event.EntityID, "", false); err != nil {
				return err
			}
		}
	}
	if err = b.Store.CheckpointHistory(job.ID, job.Generation, cursor, next, len(snapshots), ""); err != nil {
		return err
	}
	b.Hub.Notify()
	return nil
}
