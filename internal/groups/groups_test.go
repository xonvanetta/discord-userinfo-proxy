package groups

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/xonvanetta/discord-userinfo-proxy/internal/discord"
)

const testGuildID = "100"

type fakeLister struct {
	mu    sync.Mutex
	roles []*discordgo.Role
	err   error
	calls atomic.Int32
	delay time.Duration

	memberRoles []string
	memberErr   error
}

func (f *fakeLister) MemberRoles(_ context.Context, _ string) ([]string, error) {
	return f.memberRoles, f.memberErr
}

func (f *fakeLister) GuildRoles(_ context.Context) ([]*discordgo.Role, error) {
	f.calls.Add(1)

	time.Sleep(f.delay)

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.roles, f.err
}

func (f *fakeLister) set(roles []*discordgo.Role, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.roles = roles
	f.err = err
}

func baseRoles() []*discordgo.Role {
	return []*discordgo.Role{
		{ID: "100", Name: "@everyone"},
		{ID: "1", Name: "Viewers"},
		{ID: "2", Name: "Editors"},
		{ID: "3", Name: "Editors"},
		{ID: "4", Name: "Booster", Managed: true},
		{ID: "8", Name: "EDITORS"},
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newNamed(lister *fakeLister, ttl time.Duration) (*Named, *clock) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	n, err := New(lister, testGuildID, ttl)
	if err != nil {
		panic(err)
	}

	n.now = clk.now

	return n, clk
}

func TestNewRejectsNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		_, err := New(&fakeLister{}, testGuildID, ttl)
		if err == nil {
			t.Errorf("New(ttl=%s) error = nil, want error", ttl)
		}
	}
}

func TestGroupsResolvesMemberRoles(t *testing.T) {
	lister := &fakeLister{roles: baseRoles(), memberRoles: []string{"2", "1", "100"}}
	n, _ := newNamed(lister, time.Minute)

	got, err := n.Groups(context.Background(), "user")
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}

	if !reflect.DeepEqual(got, []string{"editors", "viewers"}) {
		t.Errorf("groups = %v, want [editors viewers]", got)
	}
}

func TestGroupsMemberErrors(t *testing.T) {
	boom := errors.New("boom")

	for _, memberErr := range []error{discord.ErrNotMember, boom} {
		lister := &fakeLister{roles: baseRoles(), memberErr: memberErr}
		n, _ := newNamed(lister, time.Minute)

		_, err := n.Groups(context.Background(), "user")
		if !errors.Is(err, memberErr) {
			t.Errorf("Groups() error = %v, want it to wrap %v", err, memberErr)
		}
	}
}

func mustGroups(t *testing.T, n *Named, ids ...string) []string {
	t.Helper()

	got, err := n.names(context.Background(), ids)
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}

	return got
}

func TestNamedFiltersAndSorts(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{name: "names sorted and deduplicated", ids: []string{"2", "1", "3"}, want: []string{"editors", "viewers"}},
		{name: "names lowercased, case variants merge", ids: []string{"2", "8"}, want: []string{"editors"}},
		{name: "everyone excluded", ids: []string{"100", "1"}, want: []string{"viewers"}},
		{name: "managed excluded", ids: []string{"4"}, want: []string{}},
		{name: "no roles is empty not nil", ids: nil, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, _ := newNamed(&fakeLister{roles: baseRoles()}, time.Minute)

			got := mustGroups(t, n, tt.ids...)
			if got == nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("groups = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNamedCachesWithinTTL(t *testing.T) {
	lister := &fakeLister{roles: baseRoles()}
	n, clk := newNamed(lister, time.Minute)

	mustGroups(t, n, "1")
	clk.advance(59 * time.Second)
	mustGroups(t, n, "1")

	if got := lister.calls.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1 within the TTL", got)
	}

	clk.advance(2 * time.Second)
	mustGroups(t, n, "1")

	if got := lister.calls.Load(); got != 2 {
		t.Errorf("fetches = %d, want 2 after the TTL", got)
	}
}

func TestNamedPicksUpRenameAfterTTL(t *testing.T) {
	lister := &fakeLister{roles: baseRoles()}
	n, clk := newNamed(lister, time.Minute)

	mustGroups(t, n, "1")

	renamed := baseRoles()
	renamed[1].Name = "Watchers"
	lister.set(renamed, nil)
	clk.advance(time.Minute)

	got := mustGroups(t, n, "1")
	if !reflect.DeepEqual(got, []string{"watchers"}) {
		t.Errorf("groups = %v, want the renamed role", got)
	}
}

func TestNamedUnknownRoleRefetchIsRateLimited(t *testing.T) {
	lister := &fakeLister{roles: baseRoles()}
	n, clk := newNamed(lister, time.Hour)

	mustGroups(t, n, "1")

	created := append(baseRoles(), &discordgo.Role{ID: "5", Name: "New"})
	lister.set(created, nil)
	clk.advance(11 * time.Second)

	got := mustGroups(t, n, "5")
	if !reflect.DeepEqual(got, []string{"new"}) {
		t.Fatalf("groups = %v, want the new role after one refetch", got)
	}

	if calls := lister.calls.Load(); calls != 2 {
		t.Fatalf("fetches = %d, want 2", calls)
	}

	// A role that does not exist keeps being unknown. It must not refetch every login.
	clk.advance(11 * time.Second)
	mustGroups(t, n, "6")
	clk.advance(5 * time.Second)
	mustGroups(t, n, "6")

	if calls := lister.calls.Load(); calls != 3 {
		t.Errorf("fetches = %d, want 3: one refetch for the first miss, none within 10 s", calls)
	}

	clk.advance(10 * time.Second)
	mustGroups(t, n, "6")

	if calls := lister.calls.Load(); calls != 4 {
		t.Errorf("fetches = %d, want 4 once the interval has passed", calls)
	}
}

func TestNamedKnownExcludedRoleDoesNotTriggerRefetch(t *testing.T) {
	lister := &fakeLister{roles: baseRoles()}
	n, clk := newNamed(lister, time.Hour)

	mustGroups(t, n, "4")
	clk.advance(time.Minute)
	mustGroups(t, n, "4")

	if calls := lister.calls.Load(); calls != 1 {
		t.Errorf("fetches = %d, want 1: a managed role is known", calls)
	}
}

func TestNamedFailsClosedWithoutUsableCache(t *testing.T) {
	boom := errors.New("discord down")

	t.Run("cold cache", func(t *testing.T) {
		n, _ := newNamed(&fakeLister{err: boom}, time.Minute)

		_, err := n.names(context.Background(), []string{"1"})
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want the fetch error", err)
		}
	})

	t.Run("expired cache", func(t *testing.T) {
		lister := &fakeLister{roles: baseRoles()}
		n, clk := newNamed(lister, time.Minute)
		mustGroups(t, n, "1")

		lister.set(nil, boom)
		clk.advance(time.Minute)

		got, err := n.names(context.Background(), []string{"1"})
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, groups = %v, want the fetch error and no stale names", err, got)
		}
	})

	t.Run("recovers after an error", func(t *testing.T) {
		lister := &fakeLister{err: boom}
		n, _ := newNamed(lister, time.Minute)

		_, err := n.names(context.Background(), []string{"1"})
		if err == nil {
			t.Fatal("want an error while Discord is down")
		}

		lister.set(baseRoles(), nil)

		got := mustGroups(t, n, "1")
		if !reflect.DeepEqual(got, []string{"viewers"}) {
			t.Errorf("groups = %v", got)
		}
	})
}

func TestNamedUnknownRefetchFailureKeepsCache(t *testing.T) {
	lister := &fakeLister{roles: baseRoles()}
	n, clk := newNamed(lister, time.Hour)
	mustGroups(t, n, "1")

	lister.set(nil, errors.New("discord down"))
	clk.advance(11 * time.Second)

	got, err := n.names(context.Background(), []string{"1", "5"})
	if err != nil {
		t.Fatalf("err = %v, want the cached result within the TTL", err)
	}

	if !reflect.DeepEqual(got, []string{"viewers"}) {
		t.Errorf("groups = %v, want only the cached role", got)
	}
}

func TestNamedColdCacheSharesOneFetch(t *testing.T) {
	lister := &fakeLister{roles: baseRoles(), delay: 50 * time.Millisecond}
	n, err := New(lister, testGuildID, time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup

	for range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, err := n.names(context.Background(), []string{"1"})
			if err != nil {
				t.Errorf("Groups: %v", err)
			}
		}()
	}

	wg.Wait()

	if got := lister.calls.Load(); got != 1 {
		t.Errorf("fetches = %d, want 1 for concurrent logins on a cold cache", got)
	}
}
