// Package server exposes the userinfo endpoint Dex's oauth connector calls.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/xonvanetta/discord-userinfo-proxy/internal/discord"
	"github.com/xonvanetta/discord-userinfo-proxy/internal/groups"
)

// Discord is the part of the Discord client the handler needs.
type Discord interface {
	CurrentUser(ctx context.Context, bearer string) (*discordgo.User, error)
}

// userinfo is the response body: the Discord user with the groups claim added.
type userinfo struct {
	*discordgo.User
	Groups []string `json:"groups"`
}

// Handler serves /userinfo and /healthz.
type Handler struct {
	discord Discord
	groups  groups.Resolver
	log     *slog.Logger
}

// New returns a Handler. The Discord client is bound to one guild.
func New(d Discord, resolver groups.Resolver, log *slog.Logger) *Handler {
	return &Handler{discord: d, groups: resolver, log: log}
}

// Routes returns the HTTP mux.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /userinfo", h.userinfo)
	// No Discord call here, so a Discord outage does not restart the pod.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

func (h *Handler) userinfo(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	userID := ""
	outcome := "ok"
	status := http.StatusOK

	defer func() {
		h.log.Info("userinfo", "user_id", userID, "outcome", outcome, "status", status,
			"latency_ms", time.Since(start).Milliseconds())
	}()

	w.Header().Set("Cache-Control", "no-store")

	// respond sends a bare status with no body. The reason is only logged, so
	// clients cannot distinguish failure causes beyond the status class.
	respond := func(code int, reason string) {
		status = code
		outcome = reason
		w.WriteHeader(code)
	}

	bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	bearer = strings.TrimSpace(bearer)
	if !found || bearer == "" {
		respond(http.StatusForbidden, "missing_bearer")
		return
	}

	user, err := h.discord.CurrentUser(r.Context(), bearer)
	if err != nil {
		if errors.Is(err, discord.ErrUnauthorized) {
			respond(http.StatusForbidden, "token_rejected")
			return
		}

		respond(http.StatusInternalServerError, "user_lookup_failed")
		return
	}
	userID = user.ID

	names, err := h.groups.Groups(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, discord.ErrNotMember) {
			respond(http.StatusForbidden, "not_member")
			return
		}

		// Rate limits, outages and malformed responses all fail closed: never answer
		// 200 with guessed groups.
		respond(http.StatusInternalServerError, "group_lookup_failed")
		return
	}

	body, err := json.Marshal(userinfo{User: user, Groups: names})
	if err != nil {
		respond(http.StatusInternalServerError, "encode_failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(body)
	if err != nil {
		h.log.Warn("write response", "error", err)
	}
}
