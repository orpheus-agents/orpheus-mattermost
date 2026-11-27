package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
)

type channelReplaySource struct {
	MM
	channels []mattermost.Channel
	replay   func(context.Context, string, time.Time) ([]mattermost.Post, error)
}

func (s *channelReplaySource) Channels(context.Context) ([]mattermost.Channel, error) {
	return slices.Clone(s.channels), nil
}
func (s *channelReplaySource) Posts(ctx context.Context, channel string, from time.Time) ([]mattermost.Post, error) {
	return s.replay(ctx, channel, from)
}

func TestChannelReplayWindow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sinceDays, days int
		alreadyScanned  bool
	}{
		{name: "startup", sinceDays: 30, days: 30},
		{name: "periodic", sinceDays: 30, days: 7, alreadyScanned: true},
		{name: "recent workflow", sinceDays: 2, days: 2, alreadyScanned: true},
		{name: "equal boundaries", sinceDays: 7, days: 7, alreadyScanned: true},
		{name: "future cutover", sinceDays: -1, days: -1, alreadyScanned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := discoveryRuntime(t, &runtimeAPI{})
				now := time.Now()
				r.cfg.Workflows[0].Since = now.Add(-time.Duration(tc.sinceDays) * 24 * time.Hour)
				const channel = "cccccccccccccccccccccccccc"
				if tc.alreadyScanned {
					r.scanned[channel] = now.Add(-r.cfg.Workflows[0].PollInterval.Value())
				}
				calls := 0
				r.Engine.MM = &channelReplaySource{
					channels: []mattermost.Channel{{ID: channel, Type: "O"}},
					replay: func(_ context.Context, id string, from time.Time) ([]mattermost.Post, error) {
						calls++
						if id != channel || !from.Equal(now.Add(-time.Duration(tc.days)*24*time.Hour)) {
							t.Fatalf("unexpected replay: channel=%s from=%s", id, from)
						}
						return nil, nil
					},
				}
				r.scan(t.Context(), r.cfg, nil, 0)
				if calls != 1 || !r.scanned[channel].Equal(now) || !r.ready.Load() {
					t.Fatal("replay was skipped or not recorded", calls, r.scanned[channel])
				}
				r.scan(t.Context(), r.cfg, nil, 0)
				if calls != 1 {
					t.Fatal("channel replay ignored its polling interval")
				}
			})
		})
	}
}

func TestChannelReplayWindowsAreIndependentAndRetryStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := discoveryRuntime(t, &runtimeAPI{})
		cutover := time.Now().Add(-30 * 24 * time.Hour)
		r.cfg.Workflows[0].Since = cutover
		const first, second, added = "aaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccc"
		calls := map[string]int{}
		failSecond := true
		source := &channelReplaySource{
			channels: []mattermost.Channel{{ID: first, Type: "O"}, {ID: second, Type: "O"}},
			replay: func(_ context.Context, channel string, from time.Time) ([]mattermost.Post, error) {
				calls[channel]++
				want := cutover
				if channel == first && calls[channel] > 1 || channel == second && calls[channel] > 2 {
					want = time.Now().Add(-7 * 24 * time.Hour)
				}
				if !from.Equal(want) {
					t.Fatalf("channel=%s call=%d: want %s, got %s", channel, calls[channel], want, from)
				}
				if channel == second && failSecond {
					return nil, errors.New("later channel page failed")
				}
				return nil, nil
			},
		}
		r.Engine.MM = source
		r.scan(t.Context(), r.cfg, nil, 0)
		if r.scanned[first].IsZero() || !r.scanned[second].IsZero() || r.ready.Load() {
			t.Fatal("failed replay advanced the second channel or discarded the first channel's success")
		}
		failSecond = false
		time.Sleep(r.cfg.Workflows[0].PollInterval.Value())
		r.scan(t.Context(), r.cfg, nil, 0)
		if calls[first] != 2 || calls[second] != 2 || r.scanned[second].IsZero() || !r.ready.Load() {
			t.Fatal("retry did not finish the startup replay", calls)
		}
		// A channel first seen later still needs the full cutover range.
		source.channels = append(source.channels, mattermost.Channel{ID: added, Type: "O"})
		time.Sleep(r.cfg.Workflows[0].PollInterval.Value())
		r.scan(t.Context(), r.cfg, nil, 0)
		if calls[first] != 3 || calls[second] != 3 || calls[added] != 1 || !r.ready.Load() {
			t.Fatal("new channel did not receive an independent startup replay", calls)
		}
	})
}

func TestChannelReplayRetriesFullRangeAfterInterruptedScheduling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := discoveryRuntime(t, &runtimeAPI{})
		w := r.cfg.Workflows[0]
		w.Since = time.Now().Add(-30 * 24 * time.Hour)
		r.cfg.Workflows[0] = w
		key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: "cccccccccccccccccccccccccc", Root: "rrrrrrrrrrrrrrrrrrrrrrrrrr"}
		for i := range maxQueuedThreads {
			queued := key
			queued.Root = fmt.Sprintf("%026d", i)
			r.jobs[queued] = &scheduled{Workflow: w}
		}
		calls := 0
		r.Engine.MM = &channelReplaySource{
			channels: []mattermost.Channel{{ID: key.Channel, Type: "O"}},
			replay: func(_ context.Context, _ string, from time.Time) ([]mattermost.Post, error) {
				calls++
				if !from.Equal(w.Since) {
					t.Error("incomplete initial scheduling narrowed the next replay", from)
				}
				return []mattermost.Post{{ID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: time.Now().UnixMilli(), Message: "@orpheus recover request"}}, nil
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			r.scan(ctx, r.cfg, nil, 0)
			close(done)
		}()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("full queue did not block initial scheduling")
		default:
		}
		cancel()
		<-done
		if !r.scanned[key.Channel].IsZero() || r.jobs[key] != nil || r.ready.Load() {
			t.Fatal("interrupted scheduling recorded a successful channel replay")
		}
		clear(r.jobs)
		r.scan(t.Context(), r.cfg, nil, 0)
		if calls != 2 || r.jobs[key] == nil || r.scanned[key.Channel].IsZero() || !r.ready.Load() {
			t.Fatal("retry did not complete the full initial replay", calls)
		}
	})
}

func TestChannelReplayWindowResetsOnReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine, _, mm, _, key := fixture(t)
		engine.SourceID = key.Source
		engine.API = &runtimeAPI{}
		type replay struct{ from, at time.Time }
		boundaries := make(chan replay, 8)
		engine.MM = &channelReplaySource{
			MM:       &schedulerSource{bot: mm.bot, channel: key.Channel, events: make(chan mattermost.Event)},
			channels: []mattermost.Channel{{ID: key.Channel, Type: "O"}},
			replay: func(_ context.Context, _ string, from time.Time) ([]mattermost.Post, error) {
				boundaries <- replay{from, time.Now()}
				return nil, nil
			},
		}
		reload := make(chan config.Config, 1)
		r := &Runtime{Engine: engine, Reload: reload, Logger: slog.New(slog.DiscardHandler)}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		assertBoundary := func(startup bool) {
			t.Helper()
			synctest.Wait()
			select {
			case got := <-boundaries:
				want := got.at.Add(-7 * 24 * time.Hour)
				if startup {
					want = engine.Config.Workflows[0].Since
				}
				if !got.from.Equal(want) {
					t.Fatalf("startup=%v: want %s, got %s", startup, want, got.from)
				}
			default:
				t.Fatal("missing channel replay")
			}
		}
		interval := engine.Config.Workflows[0].PollInterval.Value() + time.Second
		assertBoundary(true)
		time.Sleep(interval)
		assertBoundary(false)
		reload <- engine.Config
		assertBoundary(true)
		time.Sleep(interval)
		assertBoundary(false)
	})
}

func TestPeriodicReplayFindsNewReplyAndKeepsOldThreadContext(t *testing.T) {
	r := discoveryRuntime(t, &runtimeAPI{})
	w := r.cfg.Workflows[0]
	w.Since = time.Now().Add(-60 * 24 * time.Hour)
	r.cfg.Workflows[0] = w
	key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: "cccccccccccccccccccccccccc", Root: "rrrrrrrrrrrrrrrrrrrrrrrrrr"}
	channel := mattermost.Channel{ID: key.Channel, Type: "O"}
	oldRoot := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: time.Now().Add(-30 * 24 * time.Hour).UnixMilli(), Message: "OLD_ROOT_CONTEXT"}
	reply := mattermost.Post{ID: "nnnnnnnnnnnnnnnnnnnnnnnnnn", RootID: key.Root, ChannelID: key.Channel, UserID: oldRoot.UserID, CreateAt: time.Now().Add(-time.Hour).UnixMilli(), Message: "@orpheus NEW_QUESTION"}
	mm := &fakeSource{bot: r.Engine.Bot.ID, posts: []mattermost.Post{oldRoot, reply}}
	api := &fakeAPI{API: &runtimeAPI{}}
	r.Engine.API = api
	r.Engine.MM = &channelReplaySource{
		MM: mm, channels: []mattermost.Channel{channel},
		replay: func(_ context.Context, _ string, from time.Time) ([]mattermost.Post, error) {
			if !from.After(time.UnixMilli(oldRoot.CreateAt)) || from.After(time.UnixMilli(reply.CreateAt)) {
				t.Fatal("test must exclude the root and include the reply", from)
			}
			return []mattermost.Post{reply}, nil
		},
	}
	r.scanned[key.Channel] = time.Now().Add(-w.PollInterval.Value())
	r.scan(t.Context(), r.cfg, nil, 0)
	if len(r.jobs) != 1 || r.jobs[key] == nil {
		t.Fatal("new reply did not enqueue its old root", r.jobs)
	}
	if err := r.Engine.Thread(t.Context(), w, key, channel, true); err != nil {
		t.Fatal(err)
	}
	if len(api.submitted) != 1 || !slices.Equal(api.submitted[0].Envelope.TriggerIDs, []string{reply.ID}) || !strings.Contains(joinedInput(api.submitted[0].Messages), oldRoot.Message) {
		t.Fatal("new reply or old context was lost", api.submitted)
	}
	if !r.cfg.Workflows[0].Since.Equal(w.Since) {
		t.Fatal("periodic replay moved the workflow input cutoff")
	}
}

func TestNamedReplyThroughWebSocketAndReplay(t *testing.T) {
	for _, websocket := range []bool{true, false} {
		t.Run(map[bool]string{true: "websocket", false: "REST"}[websocket], func(t *testing.T) {
			api := &fakeAPI{API: &runtimeAPI{}}
			r := discoveryRuntime(t, api)

			base, err := os.ReadFile("../../workflows/assistant.md")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			file := strings.Replace(string(base), "---\n", "---\ntrigger_names: [Орфей, Орфеюшка]\ntrigger_name_regex: 'орфей(?:-[0-9]+)?'\n", 1)
			if err := os.WriteFile(filepath.Join(dir, "assistant.md"), []byte(file), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WORKFLOWS_DIR", dir)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			w := cfg.Workflows[0]
			w.Since = time.Now().Add(-60 * 24 * time.Hour)
			r.cfg.Workflows[0] = w

			key := conversation.Key{Source: r.Engine.SourceID, Workflow: w.ID, Channel: "cccccccccccccccccccccccccc", Root: "rrrrrrrrrrrrrrrrrrrrrrrrrr"}
			channel := mattermost.Channel{ID: key.Channel, Type: "O"}
			root := mattermost.Post{ID: key.Root, ChannelID: key.Channel, UserID: "hhhhhhhhhhhhhhhhhhhhhhhhhh", CreateAt: time.Now().Add(-30 * 24 * time.Hour).UnixMilli(), Message: "Вчера Орфей проверил логи"}
			reply := mattermost.Post{ID: "nnnnnnnnnnnnnnnnnnnnnnnnnn", RootID: key.Root, ChannelID: key.Channel, UserID: root.UserID, CreateAt: time.Now().Add(-time.Hour).UnixMilli(), Message: "ОРФЕЙ-42, проверь логи"}
			mm := &fakeSource{bot: r.Engine.Bot.ID, posts: []mattermost.Post{root, reply}}
			r.Engine.MM = &channelReplaySource{
				MM: mm, channels: []mattermost.Channel{channel},
				replay: func(context.Context, string, time.Time) ([]mattermost.Post, error) {
					return mm.posts[1:], nil
				},
			}
			if websocket {
				r.channels[key.Channel] = channel
				r.event(mattermost.Event{Post: root})
				if len(r.jobs) != 0 {
					t.Fatal("passive root entered the live queue")
				}
				r.event(mattermost.Event{Post: reply})
			} else {
				r.scan(t.Context(), r.cfg, nil, 0)
			}
			if len(r.jobs) != 1 || r.jobs[key] == nil {
				t.Fatal("named reply failed to enqueue its thread")
			}
			if err := r.Engine.Thread(t.Context(), w, key, channel, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 1 || api.submitted[0].Session != "" ||
				!slices.Equal(api.submitted[0].Envelope.TriggerIDs, []string{reply.ID}) ||
				!strings.Contains(joinedInput(api.submitted[0].Messages), root.Message) ||
				!strings.Contains(joinedInput(api.submitted[0].Messages), reply.Message) {
				t.Fatal("named reply did not start one session with the original thread text")
			}
			followUp := reply
			followUp.ID = "ffffffffffffffffffffffffff"
			followUp.CreateAt++
			followUp.Message = "орфеюшка: уточнение"
			mm.posts = append(mm.posts, followUp)
			clear(r.scanned)
			clear(r.digests)
			clear(r.jobs)
			r.scan(t.Context(), r.cfg, nil, 0)
			if r.jobs[key] == nil {
				t.Fatal("second replay did not queue the named follow-up")
			}
			if err := r.Engine.Thread(t.Context(), w, key, channel, true); err != nil {
				t.Fatal(err)
			}
			if len(api.submitted) != 2 || api.submitted[1].Session != api.snapshot.Sessions[0].ID ||
				api.submitted[1].Envelope.Kind != "clarification" ||
				!slices.Equal(api.submitted[1].Envelope.TriggerIDs, []string{followUp.ID}) {
				t.Fatal("replay duplicated the first request or lost the named follow-up", api.submitted)
			}
		})
	}
}
