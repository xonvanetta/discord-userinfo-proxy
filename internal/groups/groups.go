// Package groups resolves the group names emitted in the userinfo groups claim from a
// user's Discord guild roles.
package groups

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Resolver returns the group names of a guild member. The result is sorted, free of
// duplicates and never nil, so the claim encodes as [] rather than null. A user outside
// the guild yields an error wrapping discord.ErrNotMember.
type Resolver interface {
	Groups(ctx context.Context, userID string) ([]string, error)
}

// unknownRoleRefreshInterval is the shortest time between two role list fetches
// triggered by a role ID missing from the cache, so unknown IDs cannot hammer Discord.
const unknownRoleRefreshInterval = 10 * time.Second

// RoleLister fetches the roles of a guild and of one of its members.
type RoleLister interface {
	GuildRoles(ctx context.Context) ([]*discordgo.Role, error)
	MemberRoles(ctx context.Context, userID string) ([]string, error)
}

// Named emits the lowercased name of every non-managed guild role the member holds. The role
// list is cached for a TTL and refetched early, rate limited, when a member holds a
// role the cache does not know.
type Named struct {
	lister  RoleLister
	guildID string
	ttl     time.Duration
	now     func() time.Time

	mu          sync.Mutex
	roles       map[string]*discordgo.Role
	fetchedAt   time.Time
	lastAttempt time.Time
}

// New returns a Named resolver for one guild. It rejects a non-positive TTL so a bad
// value fails at startup.
func New(lister RoleLister, guildID string, ttl time.Duration) (*Named, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("role cache TTL must be positive, got %s", ttl)
	}

	return &Named{lister: lister, guildID: guildID, ttl: ttl, now: time.Now}, nil
}

// Groups implements Resolver. It fails when the member cannot be fetched, or when the
// role cache is empty or past its TTL and the role list cannot be fetched, so stale
// names are never served beyond the TTL.
func (n *Named) Groups(ctx context.Context, userID string) ([]string, error) {
	roleIDs, err := n.lister.MemberRoles(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("member roles: %w", err)
	}

	return n.names(ctx, roleIDs)
}

// names maps role IDs to group names using the cached guild role list.
func (n *Named) names(ctx context.Context, roleIDs []string) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.now()

	switch {
	case n.roles == nil || now.Sub(n.fetchedAt) >= n.ttl:
		err := n.refresh(ctx)
		if err != nil {
			return nil, fmt.Errorf("refresh role cache: %w", err)
		}
	case n.hasUnknown(roleIDs) && now.Sub(n.lastAttempt) >= unknownRoleRefreshInterval:
		// The cache is still within its TTL, so a failed refetch only leaves the new
		// role out. That means fewer groups, never more, and the login proceeds.
		_ = n.refresh(ctx)
	}

	names := []string{}

	for _, id := range roleIDs {
		role, known := n.roles[id]
		if known && n.isGroup(role) {
			names = append(names, strings.ToLower(role.Name))
		}
	}

	slices.Sort(names)

	return slices.Compact(names), nil
}

// refresh replaces the cache. On error the previous cache is left untouched.
func (n *Named) refresh(ctx context.Context) error {
	// Taken before the fetch, so the TTL counts from the start of the request.
	now := n.now()
	n.lastAttempt = now

	roles, err := n.lister.GuildRoles(ctx)
	if err != nil {
		return fmt.Errorf("list guild roles: %w", err)
	}

	cache := make(map[string]*discordgo.Role, len(roles))

	for _, role := range roles {
		cache[role.ID] = role
	}

	n.roles = cache
	n.fetchedAt = now

	return nil
}

// isGroup reports whether a role is emitted as a group.
func (n *Named) isGroup(role *discordgo.Role) bool {
	// @everyone has the guild's ID and every member holds it.
	if role.ID == n.guildID {
		return false
	}

	// Managed roles belong to bots, integrations and boosts, not to anything a person
	// was granted.
	if role.Managed {
		return false
	}

	return role.Name != ""
}

func (n *Named) hasUnknown(roleIDs []string) bool {
	for _, id := range roleIDs {
		_, known := n.roles[id]
		if !known {
			return true
		}
	}

	return false
}
