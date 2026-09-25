//go:build live

package service

import (
	"bytes"
	"context"
	"encoding/json"

	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-mattermost/internal/agentbox"
	"github.com/orpheus-agents/orpheus-mattermost/internal/attachments"
	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
	"github.com/orpheus-agents/orpheus-mattermost/internal/delivery"
	"github.com/orpheus-agents/orpheus-mattermost/internal/mattermost"
	"github.com/orpheus-agents/orpheus-mattermost/internal/orpheus"
)

// The live credentials belong to the bot. Represent only the trigger's author
// as a human locally; linked posts, metadata and file bytes use the real API.
type liveContextSource struct{ *mattermost.Client }

func (s liveContextSource) User(ctx context.Context, id string) (mattermost.User, error) {
	if id == "hhhhhhhhhhhhhhhhhhhhhhhhhh" {
		return mattermost.User{ID: id, Username: "live-human"}, nil
	}
	return s.Client.User(ctx, id)
}

// Manual acceptance test. Neither credentials nor a network service are needed in CI.
func TestLiveOrpheusImagesAndOutput(t *testing.T) {
	if os.Getenv("ORPHEUS_TEST_URL") == "" {
		t.Skip("manual Orpheus environment is not configured")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	t.Setenv("ORPHEUS_BASE_URL", "http://orpheus:8080")
	t.Setenv("WORKFLOWS_DIR", "../../workflows")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orpheus.BaseURL = os.Getenv("ORPHEUS_TEST_URL")
	cfg.Workflows[0].Mattermost.BaseURL = os.Getenv("MM_TEST_URL")
	botID := os.Getenv("MM_TEST_BOT_ID")

	cfg.Workflows[0].Profile = "live"
	cfg.Workflows[0].RunTimeoutSeconds = 240
	mm := mattermost.New(cfg.Workflows[0].Mattermost.BaseURL, os.Getenv("MATTERMOST_BOT_TOKEN"), 30*time.Second)
	channel, err := mm.Channel(ctx, os.Getenv("MM_TEST_CHANNEL_ID"))
	if err != nil || channel.Name != "dev-test-group" {
		t.Fatal("test channel guard", err)
	}
	api, err := orpheus.New(cfg, os.Getenv("ORPHEUS_API_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := agentbox.New(cfg, api, mm)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workflows[0].SandboxTemplate = "codex"
	w := cfg.Workflows[0]
	root, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, Message: "[orpheus-mattermost live test] Сквозная проверка изображений и файлов. Тред будет удалён."})
	if err != nil {
		t.Fatal(err)
	}
	cleanupThread := func(rootID string) {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			posts, err := mm.Thread(cleanup, rootID)
			if err == nil {
				for i := len(posts) - 1; i >= 0; i-- {
					_ = mm.Delete(cleanup, posts[i].ID)
				}
			}
		})
	}
	cleanupThread(root.ID)
	key := conversation.Key{Source: w.Mattermost.Source(botID), Workflow: w.ID, Channel: channel.ID, Root: root.ID}
	addImage := func(second bool, rootID string) (mattermost.Post, attachments.InputFile) {
		t.Helper()
		img := image.NewRGBA(image.Rect(0, 0, 256, 256))
		for y := range 256 {
			for x := range 256 {
				c := color.RGBA{255, 255, 255, 255}
				if second {
					if (x-128)*(x-128)+(y-128)*(y-128) < 80*80 {
						c = color.RGBA{0, 0, 255, 255}
					}
				} else if y > 40 && y < 210 && x > 128-(y-40)/2 && x < 128+(y-40)/2 {
					c = color.RGBA{255, 0, 0, 255}
				}
				img.Set(x, y, c)
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		file, err := mm.Upload(ctx, channel.ID, uuid.NewString()+".png", bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		post, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, RootID: rootID, Message: "Тестовое изображение.", FileIDs: []string{file.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return post, attachments.InputFile{PostID: post.ID, FileID: file.ID, Origin: "thread", Name: file.Name, MIME: file.MIME, Size: file.Size, Path: attachments.InputPath(file.ID, file.Name), ChannelID: channel.ID}
	}
	first, file := addImage(false, root.ID)
	env := conversation.Envelope{Schema: 1, Source: key.Source, Workflow: key.Workflow, Revision: w.EffectiveRevision, Channel: key.Channel, Root: key.Root, Anchor: first.ID, Kind: "initial", TriggerIDs: []string{first.ID}, Render: conversation.Render{Version: 1, MaxChars: w.MaxPostChars, Commentary: true}, Request: attachments.Request{Schema: 1, BotID: botID, SourceID: key.Source, ChannelID: key.Channel, RootID: key.Root, AnchorID: first.ID, Files: []attachments.InputFile{file}, Limits: w.Files}}
	prompt := "Open the image using view_image at " + file.Path + ". Describe its color and shape in English. Write that description to report.txt in the current run outbox. Reply with the same description. Before the final answer, run a shell sleep for 20 seconds to allow a follow-up request. Do not use network APIs."
	text := prompt
	accepted, err := api.Submit(ctx, w, env, []conversation.InputMessage{{Text: "The image is already installed in the current run sandbox."}, {Text: "Use the prepared image path in the next request."}, {Text: text}}, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var sandboxID string
	var clarificationPath, clarificationManifest string
	clarified := false
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if sandboxID != "" {
			_, _ = box.SDK.Sandboxes.Kill(cleanup, sandboxID)
		} else {
			sessions, err := api.Sessions(cleanup, key.Namespace(), key.External())
			if err == nil {
				for _, s := range sessions {
					if s.SandboxID != "" {
						_, _ = box.SDK.Sandboxes.Kill(cleanup, s.SandboxID)
					}
				}
			}
		}
	})
	await := func(rid string) (conversation.Session, conversation.Run) {
		t.Helper()
		for ctx.Err() == nil {
			snapshot, err := api.Snapshot(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range snapshot.Sessions {
				sandboxID = s.SandboxID

				for _, r := range s.Runs {
					if r.ID == accepted.RunID && r.Status == "running" && !clarified {
						post, extra := addImage(true, root.ID)
						follow := env
						follow.Anchor = post.ID
						follow.TriggerIDs = []string{post.ID}
						follow.Kind = "clarification"
						follow.Request.AnchorID = post.ID
						follow.Request.Files = []attachments.InputFile{extra}
						if err := box.Prepare(ctx, s, r, w, follow.Request); err != nil {
							t.Fatal("clarification preparation", err)
						}
						followText := "Also open the additional image with view_image: " + extra.Path + ". Describe BOTH images' colors and shapes in English in report.txt and in your final answer. The first image remains at " + file.Path
						if _, err := api.Submit(ctx, w, follow, []conversation.InputMessage{{Text: "An additional image is now available in this run."}, {Text: "Keep the first image in the final comparison."}, {Text: followText}}, s.ID, r.ID, r.ID); err != nil {
							t.Fatal(err)
						}
						clarified = true
						clarificationPath = extra.Path
						clarificationManifest = attachments.ManifestPath(post.ID)
						t.Log("live clarification file installed and accepted during run")
					}

					if r.ID == rid && r.Terminal() && s.SandboxState == "paused" {
						if r.Status != "completed" {
							t.Fatalf("run %s: %s %s", r.ID, r.Status, r.Error)
						}
						return s, r
					}
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		t.Fatal(ctx.Err())
		return conversation.Session{}, conversation.Run{}
	}
	session, run := await(accepted.RunID)
	check := func(s conversation.Session, r conversation.Run, want1, want2 string) {
		t.Helper()
		var answer string
		for _, m := range s.Messages {
			if m.RunID == r.ID && m.Role == "assistant" && m.Kind == "answer" {
				answer += m.Text
			}
		}
		lower := strings.ToLower(answer)
		if !strings.Contains(lower, want1) || !strings.Contains(lower, want2) {
			t.Fatalf("vision answer did not identify control image: %q", answer)
		}
		engine := &Engine{Config: cfg, MM: mm, API: api, Sandbox: box, Bot: mattermost.User{ID: botID}}
		publisher := &delivery.Publisher{MM: mm, Key: key, BotID: botID}
		if err := publisher.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if err := engine.publish(ctx, publisher, s, r); err != nil {
			t.Fatal(err)
		}
		firstPosts := make(map[string]bool, len(publisher.Posts))
		for _, post := range publisher.Posts {
			firstPosts[post.ID] = true
		}
		count := len(firstPosts)
		// Reconstruct both objects: a restart must not rely on local receipts.
		engine = &Engine{Config: cfg, MM: mm, API: api, Sandbox: box, Bot: mattermost.User{ID: botID}}
		publisher = &delivery.Publisher{MM: mm, Key: key, BotID: botID}
		if err := publisher.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if err := engine.publish(ctx, publisher, s, r); err != nil {
			t.Fatal(err)
		}
		var added, missing []string
		for _, post := range publisher.Posts {
			if !firstPosts[post.ID] {
				var receipt delivery.Receipt
				_ = json.Unmarshal(post.Props[delivery.Property], &receipt)
				added = append(added, post.ID+":"+receipt.RunID+":"+receipt.MessageID)
			}
			delete(firstPosts, post.ID)
		}
		for id := range firstPosts {
			missing = append(missing, id)
		}
		if len(added) > 0 || len(missing) > 0 {
			t.Fatalf("replay changed posts: before=%d after=%d added=%v missing=%v", count, len(publisher.Posts), added, missing)
		}
		found := false
		for _, post := range publisher.Posts {
			var receipt delivery.Receipt
			if json.Unmarshal(post.Props[delivery.Property], &receipt) == nil && receipt.RunID == r.ID && receipt.MessageID == r.FinalMessageID && len(post.FileIDs) > 0 {
				found = true
				info, err := mm.File(ctx, post.FileIDs[0])
				if err != nil || info.PostID != post.ID {
					t.Fatal("output not attached", err)
				}
			}
		}
		if !found {
			t.Fatal("final answer has no output attachment")
		}
	}
	if !clarified {
		t.Fatal("clarification was not exercised")
	}
	check(session, run, "red", "triangle")
	check(session, run, "blue", "circle")
	t.Log("clarification vision verified")
	t.Log("initial image recognized; final file verified; replay stable")
	second, secondFile := addImage(true, root.ID)
	env.Anchor = second.ID
	env.TriggerIDs = []string{second.ID}
	env.Request.AnchorID = second.ID
	env.Request.Files = []attachments.InputFile{secondFile}
	env.Request.PreviousIndex = attachments.IndexPath(run.ID)
	env.Request.DeliveredBatches = []string{clarificationManifest}
	text = "Open all three images with view_image: " + file.Path + ", " + clarificationPath + " and " + secondFile.Path + ". Describe each image's color and shape in English, identifying it by its path. Write the descriptions to report.txt in the current outbox and reply with the same descriptions. Before your final answer, run a shell sleep for 30 seconds to allow a worker restart. Do not use network APIs."
	next, err := api.Submit(ctx, w, env, []conversation.InputMessage{{Text: text}}, session.ID, "", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, run = await(next.RunID)
	if session.ID != accepted.SessionID || len(session.Runs) != 2 || run.ID != next.RunID {
		t.Fatal("continuation duplicated or replaced the accepted run")
	}
	check(session, run, "red", "triangle")
	check(session, run, "blue", "circle")
	t.Log("paused session resumed; old and new image paths usable; separate run outbox verified")

	linkedRoot, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, Message: "[orpheus-mattermost live test] Linked image fixture; automatic cleanup."})
	if err != nil {
		t.Fatal(err)
	}
	cleanupThread(linkedRoot.ID)
	linkedPost, linkedFile := addImage(false, linkedRoot.ID)
	trigger, err := mm.Create(ctx, mattermost.CreatePost{ChannelID: channel.ID, RootID: root.ID, Message: "@orpheus Open the image from " + w.Mattermost.BaseURL + "/pl/" + linkedPost.ID + ". Describe its color and shape in English. Write the description to report.txt in the current outbox and reply with the same description. Do not use network APIs."})
	if err != nil {
		t.Fatal(err)
	}
	trigger.UserID = "hhhhhhhhhhhhhhhhhhhhhhhhhh"
	snapshot, err := api.Snapshot(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	builder := conversation.Builder{Source: liveContextSource{mm}, Config: cfg, Workflow: w, Bot: mattermost.User{ID: botID, Username: "orpheus"}}
	linkedEnv, linkedMessages, err := builder.BuildMessages(ctx, key, channel, []mattermost.Post{trigger}, snapshot, false, time.Now().Add(w.MessageBatchWindow.Value()))
	if err != nil {
		t.Fatal(err)
	}
	if len(linkedEnv.Request.Files) != 1 || linkedEnv.Request.Files[0].FileID != linkedFile.FileID || linkedEnv.Request.Files[0].Origin != "linked_thread" {
		t.Fatal("linked image was not included in the input request")
	}
	linkedEnv.Request.PreviousIndex = attachments.IndexPath(run.ID)
	next, err = api.Submit(ctx, w, linkedEnv, linkedMessages, session.ID, "", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, run = await(next.RunID)
	check(session, run, "red", "triangle")
	t.Log("linked-thread image expanded, downloaded and recognized on continuation; output attached")
}
