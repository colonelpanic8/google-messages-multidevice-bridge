package bridge

import (
	"context"
	"fmt"
	stdlog "log"
	"time"
)

// outageNoticeAfter is how long the connection must stay down before anyone is
// told; brief drops recover on their own.
const outageNoticeAfter = 15 * time.Minute

// outageSettle is how long the connection must stay up before an outage ends,
// so a phone that answers once in the middle of one does not split it.
const outageSettle = 5 * time.Minute

func outageState(state string) bool {
	switch state {
	case "degraded", "connecting", "connection_failed":
		return true
	}
	return false
}

// trackOutage advances the current outage to now. It returns the outage that
// was just announced, or the one that just ended and when the connection that
// ended it came up.
func (b *Bridge) trackOutage(now time.Time) (announced, ended *Outage, restored time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, st := b.status.Outage, b.status
	switch {
	case outageState(st.State):
		if o == nil {
			o = &Outage{Since: st.StateSince}
		}
		if !o.Announced && now.Sub(o.Since) >= outageNoticeAfter {
			o = &Outage{Since: o.Since, Announced: true}
			announced = o
		}
		b.status.Outage = o
	case o != nil && st.State == "connected" && now.Sub(st.StateSince) >= outageSettle:
		b.status.Outage = nil
		return nil, o, st.StateSince
	}
	return announced, nil, time.Time{}
}

// WatchConnection follows outages until ctx ends. It logs each one, and
// announces through notifier, when there is one, those that last
// outageNoticeAfter and their end.
func (b *Bridge) WatchConnection(ctx context.Context, notifier PushNotifier) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		announced, ended, restored := b.trackOutage(time.Now())
		label := "Messages"
		if b.Network() == "whatsapp" {
			label = "WhatsApp"
		}
		var title, body string
		switch {
		case announced != nil:
			stdlog.Printf("%s outage since %s", b.Network(), announced.Since.Format(time.RFC3339))
			title = label + " connection lost"
			body = fmt.Sprintf("Your phone has not been reachable since %s. Messages may be delayed until it is.", announced.Since.Local().Format(time.Kitchen))
		case ended != nil:
			lasted := restored.Sub(ended.Since).Round(time.Second)
			stdlog.Printf("%s outage over after %s", b.Network(), lasted)
			if !ended.Announced {
				continue
			}
			title = label + " connection restored"
			body = fmt.Sprintf("Your phone is reachable again after %s.", lasted.Round(time.Minute))
		default:
			continue
		}
		b.Hub.Notify()
		if notifier != nil {
			send, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = notifier.Send(send, title, body, "connection", "")
			cancel()
		}
	}
}
