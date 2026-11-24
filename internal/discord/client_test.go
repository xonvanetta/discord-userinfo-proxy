package discord

import (
	"net/http"
	"testing"
)

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "valid", config: Config{BotToken: "token", GuildID: "123"}},
		{name: "blank token", config: Config{BotToken: "  ", GuildID: "123"}, wantErr: true},
		{name: "missing guild", config: Config{BotToken: "token"}, wantErr: true},
		{name: "non numeric guild", config: Config{BotToken: "token", GuildID: "12a"}, wantErr: true},
		{name: "path injection in guild", config: Config{BotToken: "token", GuildID: "1/../2"}, wantErr: true},
		{name: "guild with spaces", config: Config{BotToken: "token", GuildID: " 123 "}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.config, "test-agent", http.DefaultClient)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
