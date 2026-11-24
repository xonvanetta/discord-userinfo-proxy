package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xonvanetta/discord-userinfo-proxy/internal/discord"
	"github.com/xonvanetta/discord-userinfo-proxy/internal/groups"
)

const (
	guildID  = "100"
	userID   = "42"
	botToken = "bot-secret"
	userBody = `{"id":"42","username":"fabbe","global_name":null,"email":"a@b.se","verified":true,"avatar":"x"}`
)

// fakeDiscord answers the two endpoints the proxy calls. memberStatus and memberBody
// control the guild member lookup; userStatus controls /users/@me.
type fakeDiscord struct {
	userStatus   int
	userBody     string
	memberStatus int
	memberBody   string
	rolesStatus  int
	rolesBody    string

	botAuth     string
	userAuth    string
	userCalls   atomic.Int32
	memberCalls atomic.Int32
	rolesCalls  atomic.Int32
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// discordgo targets API v9.
	switch {
	case r.URL.Path == "/api/v9/users/@me":
		f.userCalls.Add(1)
		f.userAuth = r.Header.Get("Authorization")
		if r.Header.Get("Authorization") != "Bearer user-token" && f.userStatus == http.StatusOK {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.userStatus)
		_, _ = io.WriteString(w, f.userBody)
	case r.URL.Path == "/api/v9/guilds/"+guildID+"/members/"+userID:
		f.memberCalls.Add(1)
		f.botAuth = r.Header.Get("Authorization")
		w.WriteHeader(f.memberStatus)
		_, _ = io.WriteString(w, f.memberBody)
	case r.URL.Path == "/api/v9/guilds/"+guildID+"/roles":
		f.rolesCalls.Add(1)
		w.WriteHeader(f.rolesStatus)
		_, _ = io.WriteString(w, f.rolesBody)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFake() *fakeDiscord {
	return &fakeDiscord{
		userStatus:   http.StatusOK,
		userBody:     userBody,
		memberStatus: http.StatusOK,
		memberBody:   `{"roles":["1","2","9"]}`,
		rolesStatus:  http.StatusOK,
		rolesBody: `[` +
			`{"id":"100","name":"@everyone","managed":false},` +
			`{"id":"1","name":"Viewers","managed":false},` +
			`{"id":"2","name":"Editors","managed":false},` +
			`{"id":"9","name":"Bot Role","managed":true}]`,
	}
}

// rewriteTransport sends every request, whatever host discordgo built, to the fake.
type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host

	return http.DefaultTransport.RoundTrip(clone)
}

func newHandler(t *testing.T, fake *fakeDiscord) http.Handler {
	t.Helper()

	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	httpClient := &http.Client{Timeout: 2 * time.Second, Transport: rewriteTransport{target: target}}

	client, err := discord.New(discord.Config{BotToken: botToken, GuildID: guildID}, "test-agent", httpClient)
	if err != nil {
		t.Fatalf("discord.New: %v", err)
	}

	resolver, err := groups.New(client, guildID, time.Minute)
	if err != nil {
		t.Fatalf("groups.New: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return New(client, resolver, log).Routes()
}

func get(h http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/userinfo", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestUserinfoStatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeDiscord)
		auth   string
		want   int
	}{
		{name: "member with known roles", mutate: func(*fakeDiscord) {}, auth: "Bearer user-token", want: http.StatusOK},
		{name: "member with only unknown roles", mutate: func(f *fakeDiscord) { f.memberBody = `{"roles":["77"]}` }, auth: "Bearer user-token", want: http.StatusOK},
		{name: "member with empty roles", mutate: func(f *fakeDiscord) { f.memberBody = `{"roles":[]}` }, auth: "Bearer user-token", want: http.StatusOK},
		{name: "no authorization header", mutate: func(*fakeDiscord) {}, auth: "", want: http.StatusForbidden},
		{name: "not a bearer scheme", mutate: func(*fakeDiscord) {}, auth: "Basic abc", want: http.StatusForbidden},
		{name: "empty bearer", mutate: func(*fakeDiscord) {}, auth: "Bearer ", want: http.StatusForbidden},
		{name: "discord rejects user token", mutate: func(*fakeDiscord) {}, auth: "Bearer wrong", want: http.StatusForbidden},
		{name: "user lookup 500", mutate: func(f *fakeDiscord) { f.userStatus = http.StatusInternalServerError }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		{name: "user lookup rate limited", mutate: func(f *fakeDiscord) { f.userStatus = http.StatusTooManyRequests }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		{name: "user id not numeric", mutate: func(f *fakeDiscord) { f.userBody = `{"id":"4/../2"}` }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		{name: "user body malformed", mutate: func(f *fakeDiscord) { f.userBody = `not json` }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		{name: "not in guild", mutate: func(f *fakeDiscord) { f.memberStatus = http.StatusNotFound }, auth: "Bearer user-token", want: http.StatusForbidden},
		{name: "member lookup rate limited", mutate: func(f *fakeDiscord) { f.memberStatus = http.StatusTooManyRequests }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		{name: "member lookup 403 from discord", mutate: func(f *fakeDiscord) { f.memberStatus = http.StatusForbidden }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		{name: "member lookup 500", mutate: func(f *fakeDiscord) { f.memberStatus = http.StatusInternalServerError }, auth: "Bearer user-token", want: http.StatusInternalServerError},
		// A 200 proves membership, so a member without roles is accepted with no groups.
		{name: "member body without roles", mutate: func(f *fakeDiscord) { f.memberBody = `{}` }, auth: "Bearer user-token", want: http.StatusOK},
		{name: "roles is null", mutate: func(f *fakeDiscord) { f.memberBody = `{"roles":null}` }, auth: "Bearer user-token", want: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			tt.mutate(fake)

			rec := get(newHandler(t, fake), tt.auth)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.want, rec.Body.String())
			}

			// Every non-200 must carry no userinfo JSON, so nothing can parse as a login.
			if rec.Code != http.StatusOK && strings.Contains(rec.Body.String(), `"groups"`) {
				t.Errorf("error response contains groups: %q", rec.Body.String())
			}

			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestUserinfoBody(t *testing.T) {
	tests := []struct {
		name       string
		memberBody string
		want       string
	}{
		{name: "role names sorted, managed role excluded", memberBody: `{"roles":["2","1","9"]}`, want: `["editors","viewers"]`},
		{name: "unknown role is an empty array", memberBody: `{"roles":["77"]}`, want: `[]`},
		{name: "no roles is an empty array", memberBody: `{"roles":[]}`, want: `[]`},
		{name: "repeated role", memberBody: `{"roles":["1","1"]}`, want: `["viewers"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			fake.memberBody = tt.memberBody

			rec := get(newHandler(t, fake), "Bearer user-token")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
			}

			var got map[string]json.RawMessage
			err := json.Unmarshal(rec.Body.Bytes(), &got)
			if err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}

			if string(got["groups"]) != tt.want {
				t.Errorf("groups = %s, want %s", got["groups"], tt.want)
			}
		})
	}
}

func TestUserinfoAllRoles(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*fakeDiscord)
		wantStatus int
		wantGroups string
	}{
		{name: "names, managed role excluded", mutate: func(*fakeDiscord) {}, wantStatus: http.StatusOK, wantGroups: `["editors","viewers"]`},
		{name: "everyone excluded", mutate: func(f *fakeDiscord) { f.memberBody = `{"roles":["100"]}` }, wantStatus: http.StatusOK, wantGroups: `[]`},
		{name: "no roles", mutate: func(f *fakeDiscord) { f.memberBody = `{"roles":[]}` }, wantStatus: http.StatusOK, wantGroups: `[]`},
		{name: "roles lookup 500", mutate: func(f *fakeDiscord) { f.rolesStatus = http.StatusInternalServerError }, wantStatus: http.StatusInternalServerError},
		{name: "roles lookup rate limited", mutate: func(f *fakeDiscord) { f.rolesStatus = http.StatusTooManyRequests }, wantStatus: http.StatusInternalServerError},
		{name: "roles lookup 502", mutate: func(f *fakeDiscord) { f.rolesStatus = http.StatusBadGateway }, wantStatus: http.StatusInternalServerError},
		{name: "roles body null", mutate: func(f *fakeDiscord) { f.rolesBody = `null` }, wantStatus: http.StatusInternalServerError},
		{name: "roles body malformed", mutate: func(f *fakeDiscord) { f.rolesBody = `{` }, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			tt.mutate(fake)

			rec := get(newHandler(t, fake), "Bearer user-token")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			// One attempt only: a login must not wait on discordgo retries.
			if got := fake.rolesCalls.Load(); got != 1 {
				t.Errorf("roles calls = %d, want 1", got)
			}

			if rec.Code != http.StatusOK {
				if strings.Contains(rec.Body.String(), `"groups"`) {
					t.Errorf("error response contains groups: %q", rec.Body.String())
				}

				return
			}

			var got map[string]json.RawMessage
			err := json.Unmarshal(rec.Body.Bytes(), &got)
			if err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}

			if string(got["groups"]) != tt.wantGroups {
				t.Errorf("groups = %s, want %s", got["groups"], tt.wantGroups)
			}
		})
	}
}

func TestUserinfoPassthrough(t *testing.T) {
	fake := newFake()
	rec := get(newHandler(t, fake), "Bearer user-token")

	var got map[string]json.RawMessage
	err := json.Unmarshal(rec.Body.Bytes(), &got)
	if err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}

	// Dex builds sub and its claim mapping from these fields. The user is re-marshaled
	// from discordgo.User, so a null global_name becomes an empty string.
	want := map[string]string{
		"id":          `"42"`,
		"username":    `"fabbe"`,
		"global_name": `""`,
		"email":       `"a@b.se"`,
		"verified":    `true`,
		"avatar":      `"x"`,
		"groups":      `["editors","viewers"]`,
	}

	for key, value := range want {
		if string(got[key]) != value {
			t.Errorf("field %s = %s, want %s", key, got[key], value)
		}
	}
}

func TestNoRetries(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*fakeDiscord)
		wantStatus  int
		wantUser    int32
		wantMembers int32
	}{
		{name: "member 429", mutate: func(f *fakeDiscord) { f.memberStatus = http.StatusTooManyRequests }, wantStatus: http.StatusInternalServerError, wantUser: 1, wantMembers: 1},
		{name: "member 502", mutate: func(f *fakeDiscord) { f.memberStatus = http.StatusBadGateway }, wantStatus: http.StatusInternalServerError, wantUser: 1, wantMembers: 1},
		{name: "user 429", mutate: func(f *fakeDiscord) { f.userStatus = http.StatusTooManyRequests }, wantStatus: http.StatusInternalServerError, wantUser: 1, wantMembers: 0},
		{name: "user 502", mutate: func(f *fakeDiscord) { f.userStatus = http.StatusBadGateway }, wantStatus: http.StatusInternalServerError, wantUser: 1, wantMembers: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			tt.mutate(fake)

			// discordgo retries 429 and 502 by default and sleeps between attempts. A login
			// request must fail fast, so each upstream call happens exactly once.
			start := time.Now()
			rec := get(newHandler(t, fake), "Bearer user-token")

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if got := fake.userCalls.Load(); got != tt.wantUser {
				t.Errorf("user calls = %d, want %d", got, tt.wantUser)
			}

			if got := fake.memberCalls.Load(); got != tt.wantMembers {
				t.Errorf("member calls = %d, want %d", got, tt.wantMembers)
			}

			if time.Since(start) > time.Second {
				t.Errorf("request took %s, want a fast failure", time.Since(start))
			}
		})
	}
}

func TestTokensGoToTheRightEndpoint(t *testing.T) {
	fake := newFake()
	rec := get(newHandler(t, fake), "Bearer user-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if fake.userAuth != "Bearer user-token" {
		t.Errorf("user lookup Authorization = %q, want the user's bearer token", fake.userAuth)
	}

	if fake.botAuth != "Bot "+botToken {
		t.Errorf("member lookup Authorization = %q, want the bot token", fake.botAuth)
	}

	if strings.Contains(rec.Body.String(), botToken) {
		t.Error("response leaks the bot token")
	}
}

func TestHealthzMakesNoDiscordCall(t *testing.T) {
	fake := newFake()
	fake.userStatus = http.StatusInternalServerError
	fake.memberStatus = http.StatusInternalServerError

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	newHandler(t, fake).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 even while Discord is failing", rec.Code)
	}
}
