package mattermost

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"

	"github.com/orpheus-agents/orpheus-mattermost/internal/config"
)

// Audience returns every channel member, without filtering roles or bots.
// Counts before and after pagination detect incomplete or changing membership.
// Mattermost does not offer a transactional membership snapshot; a detected
// change fails admission and must be observed again before issuing credentials.
func (c *Client) Audience(ctx context.Context, channel string) ([]string, error) {
	if !config.ValidID(channel) {
		return nil, errors.New("invalid audience channel")
	}
	count := func() (int, error) {
		var stats struct {
			MemberCount *int `json:"member_count"`
		}
		err := c.json(ctx, "GET", "/channels/"+url.PathEscape(channel)+"/stats", nil, &stats)
		if err != nil {
			return 0, err
		}
		if stats.MemberCount == nil || *stats.MemberCount < 1 || *stats.MemberCount > 4096 {
			return 0, errors.New("invalid audience count")
		}
		return *stats.MemberCount, nil
	}
	before, err := count()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []string
	for page := 0; ; page++ {
		var members []struct {
			ChannelID string `json:"channel_id"`
			UserID    string `json:"user_id"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"200"}}
		if err := c.json(ctx, "GET", "/channels/"+url.PathEscape(channel)+"/members?"+q.Encode(), nil, &members); err != nil {
			return nil, err
		}
		if members == nil || len(members) > 200 {
			return nil, errors.New("invalid audience page")
		}
		for _, member := range members {
			if member.ChannelID != channel || !config.ValidID(member.UserID) || seen[member.UserID] {
				return nil, errors.New("inconsistent audience page")
			}
			seen[member.UserID] = true
			result = append(result, member.UserID)
		}
		if len(result) > before {
			return nil, errors.New("audience changed during pagination")
		}
		if len(members) < 200 {
			break
		}
	}
	after, err := count()
	if err != nil {
		return nil, err
	}
	if before != after || len(result) != after {
		return nil, errors.New("incomplete audience")
	}
	slices.Sort(result)
	return result, nil
}
