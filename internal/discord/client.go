// Package discord wraps the two Discord REST calls the proxy needs, using discordgo
// for requests, rate limit detection and error types.
package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
)

var (
	// ErrUnauthorized means Discord rejected the user's bearer token.
	ErrUnauthorized = errors.New("discord rejected the token")
	// ErrNotMember means the user is not a member of the guild.
	ErrNotMember = errors.New("user is not a guild member")
	// ErrRateLimited means Discord answered 429.
	ErrRateLimited = errors.New("discord rate limited the request")
	// ErrUpstream covers every other failure: transport errors, unexpected status
	// codes and malformed responses.
	ErrUpstream = errors.New("discord upstream error")
)

// Config is the Discord part of the proxy configuration.
type Config struct {
	// BotToken is the Discord bot token used for the guild member lookup.
	BotToken string
	// GuildID is the only guild whose members are accepted.
	GuildID string
}

// Client talks to the Discord API for one guild.
type Client struct {
	bot        *discordgo.Session
	guildID    string
	httpClient *http.Client
	userAgent  string
}

// New returns a Client. The bot session is reused for every member lookup. The user
// agent must follow Discord's "DiscordBot (url, version)" convention. It rejects a
// blank token or a guild ID that is not numeric, so a bad deployment fails at startup
// instead of failing at request time.
func New(config Config, userAgent string, httpClient *http.Client) (*Client, error) {
	token := strings.TrimSpace(config.BotToken)
	if token == "" {
		return nil, errors.New("bot token is required")
	}

	// The guild ID is interpolated into API paths.
	if !IsSnowflake(config.GuildID) {
		return nil, fmt.Errorf("guild ID must be numeric, got %q", config.GuildID)
	}

	bot, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create bot session: %w", err)
	}

	c := &Client{bot: bot, guildID: config.GuildID, httpClient: httpClient, userAgent: userAgent}
	c.configure(bot)

	return c, nil
}

// IsSnowflake reports whether s looks like a Discord ID: a non-empty string of
// ASCII digits. It also guards the IDs interpolated into API paths.
func IsSnowflake(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// configure makes a session fail fast. discordgo retries 429 and 502 by default and
// sleeps between attempts, which would stall a login request.
func (c *Client) configure(s *discordgo.Session) {
	s.Client = c.httpClient
	s.UserAgent = c.userAgent
	s.ShouldRetryOnRateLimit = false
	s.MaxRestRetries = 0
}

// CurrentUser calls GET /users/@me with the user's bearer token. The returned user
// has a validated, numeric ID.
func (c *Client) CurrentUser(ctx context.Context, bearer string) (*discordgo.User, error) {
	// A session per call: it holds the user's token and its own rate limit state, so
	// nothing is shared between logins.
	session, err := discordgo.New("Bearer " + bearer)
	if err != nil {
		return nil, fmt.Errorf("%w: create session: %v", ErrUpstream, err)
	}
	c.configure(session)

	defer session.Close()

	user, err := session.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		return nil, classify(err)
	}

	if user == nil {
		return nil, fmt.Errorf("%w: user response is null", ErrUpstream)
	}

	// The ID is interpolated into a request path later, so it must be digits only.
	if !IsSnowflake(user.ID) {
		return nil, fmt.Errorf("%w: user id %q is not numeric", ErrUpstream, user.ID)
	}

	return user, nil
}

// MemberRoles calls GET /guilds/{guild}/members/{user} with the bot token and
// returns the member's role IDs. A user outside the guild yields ErrNotMember.
func (c *Client) MemberRoles(ctx context.Context, userID string) ([]string, error) {
	member, err := c.bot.GuildMember(c.guildID, userID, discordgo.WithContext(ctx))
	if err != nil {
		return nil, classify(err)
	}

	// A 200 proves membership. A response without roles means no roles.
	return member.Roles, nil
}

// GuildRoles calls GET /guilds/{guild}/roles with the bot token and returns every
// role of the guild.
func (c *Client) GuildRoles(ctx context.Context) ([]*discordgo.Role, error) {
	roles, err := c.bot.GuildRoles(c.guildID, discordgo.WithContext(ctx))
	if err != nil {
		return nil, classify(err)
	}

	// Session.GuildRoles does not panic on a null body, it returns a nil slice. Reject
	// it, and null entries, so a bad response fails closed instead of meaning "no roles".
	if roles == nil || slices.Contains(roles, nil) {
		return nil, fmt.Errorf("%w: roles response is null", ErrUpstream)
	}

	return roles, nil
}

// classify maps discordgo's errors onto the package sentinels. A 404 means the guild
// member does not exist, which is the only call where the status carries that meaning.
func classify(err error) error {
	_, isRateLimit := errors.AsType[*discordgo.RateLimitError](err)
	if isRateLimit {
		return ErrRateLimited
	}

	rest, isREST := errors.AsType[*discordgo.RESTError](err)
	if isREST && rest.Response != nil {
		switch rest.Response.StatusCode {
		case http.StatusUnauthorized:
			return ErrUnauthorized
		case http.StatusNotFound:
			return ErrNotMember
		case http.StatusTooManyRequests:
			return ErrRateLimited
		}

		return fmt.Errorf("%w: status %d", ErrUpstream, rest.Response.StatusCode)
	}

	return fmt.Errorf("%w: %v", ErrUpstream, err)
}
