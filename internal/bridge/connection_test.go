package bridge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestPeriodicReconcileFetchesOnlyChangedOrStaleConversations(t *testing.T) {
	b := testBridge(t)
	convs := map[string]model.Conversation{
		"quiet": {Schema: 1, ID: "quiet", Preview: "old"},
		"busy":  {Schema: 1, ID: "busy", Preview: "old"},
		"stale": {Schema: 1, ID: "stale", Preview: "old"},
	}
	var fetched []string
	b.setProvider(&fakeProvider{
		conversations: func(context.Context) ([]provider.Snapshot, error) {
			var out []provider.Snapshot
			for _, id := range []string{"quiet", "busy", "stale"} {
				out = append(out, snapshot(t, "conversation", id, convs[id]))
			}
			return out, nil
		},
		messages: func(_ context.Context, id string) ([]provider.Snapshot, error) {
			fetched = append(fetched, id)
			return nil, nil
		},
	})
	seen := map[string]reconciled{}
	run := func(want ...string) {
		t.Helper()
		fetched = nil
		if err := b.reconcile(context.Background(), seen); err != nil {
			t.Fatal(err)
		}
		if len(fetched) != len(want) {
			t.Fatalf("fetched %v, want %v", fetched, want)
		}
		for i := range want {
			if fetched[i] != want[i] {
				t.Fatalf("fetched %v, want %v", fetched, want)
			}
		}
	}
	run("quiet", "busy", "stale")
	run()
	busy := convs["busy"]
	busy.Preview = "new"
	convs["busy"] = busy
	last := seen["stale"]
	last.at = time.Now().Add(-fullReconcileEvery)
	seen["stale"] = last
	run("busy", "stale")
	clear(seen)
	run("quiet", "busy", "stale")
}

func TestSyncBacksOffWhilePhoneIsUnresponsive(t *testing.T) {
	b := testBridge(t)
	b.syncEvery = 10 * time.Millisecond
	var calls atomic.Int32
	b.provider = &fakeProvider{conversations: func(context.Context) ([]provider.Snapshot, error) {
		calls.Add(1)
		return nil, provider.ErrUnavailable
	}}
	b.Handle(&events.ClientReady{})
	b.Handle(&events.PhoneNotResponding{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.syncLoop(ctx) }()
	defer func() { cancel(); <-done }()
	// Unbacked, this window holds about 40 attempts; doubling allows 6.
	time.Sleep(400 * time.Millisecond)
	if n := calls.Load(); n < 2 || n > 12 {
		t.Fatalf("attempts while unresponsive: %d", n)
	}
	// Asking explicitly, or the phone answering again, overrides the backoff.
	for _, prompt := range []func(){b.RequestFullSync, func() { b.Handle(&events.PhoneRespondingAgain{}) }} {
		before := calls.Load()
		prompt()
		deadline := time.Now().Add(time.Second)
		for calls.Load() == before {
			if time.Now().After(deadline) {
				t.Fatal("prompt did not sync")
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestOutageSurvivesFlapsAndAnnouncesOnce(t *testing.T) {
	b := testBridge(t)
	start := time.Now()
	set := func(state string, at time.Duration) {
		b.mu.Lock()
		b.status.State, b.status.StateSince = state, start.Add(at)
		b.mu.Unlock()
	}
	at := func(d time.Duration) (*Outage, *Outage, time.Time) { return b.trackOutage(start.Add(d)) }

	set("degraded", 0)
	if a, e, _ := at(10 * time.Minute); a != nil || e != nil {
		t.Fatal("announced too early")
	}
	if a, _, _ := at(outageNoticeAfter); a == nil || !a.Since.Equal(start) {
		t.Fatalf("not announced: %+v", a)
	}
	if a, _, _ := at(outageNoticeAfter + time.Minute); a != nil {
		t.Fatal("announced twice")
	}
	set("connected", 20*time.Minute)
	set("degraded", 21*time.Minute)
	set("connected", 30*time.Minute)
	if _, e, _ := at(30*time.Minute + outageSettle - time.Second); e != nil {
		t.Fatal("ended before settling")
	}
	_, e, restored := at(30*time.Minute + outageSettle)
	if e == nil || !e.Announced || !e.Since.Equal(start) || !restored.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("ended=%+v restored=%v", e, restored)
	}
	if b.Status().Outage != nil {
		t.Fatal("outage outlived its end")
	}
}

func TestPhoneAlertsReportConnectionStateOnly(t *testing.T) {
	b := testBridge(t)
	for _, alert := range []gmproto.AlertType{
		gmproto.AlertType_MOBILE_DATA_CONNECTION,
		gmproto.AlertType_PUSH_THROTTLE_STARTED,
		gmproto.AlertType_MOBILE_DATABASE_SYNCING,
		gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT,
	} {
		if err := b.Handle(&gmproto.UserAlertEvent{AlertType: alert}); err != nil {
			t.Fatal(err)
		}
	}
	status := b.Status()
	want := PhoneReport{Network: "mobile_data", Session: "timed_out", PushThrottled: true}
	if status.PhoneReport != want || len(status.ConnectionEvents) != 3 {
		t.Fatalf("report=%+v events=%+v", status.PhoneReport, status.ConnectionEvents)
	}
	b.Handle(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_PUSH_THROTTLE_ENDED})
	if b.Status().PhoneReport.PushThrottled {
		t.Fatal("throttle never ended")
	}
}
