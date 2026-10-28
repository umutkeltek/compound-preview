package gamesource

import (
	"context"
	"net/http"
	"time"
)

const (
	AdapterIDSteamWeb = "compound.game.steamweb"
	AdapterIDSteamSpy = "compound.game.steamspy"
)

const (
	SourceSystemSteamWeb = "steamweb"
	SourceSystemSteamSpy = "steamspy"
)

type FetchRequest struct {
	AppIDs    []int64
	Genres    []string
	Companies []string
	Now       time.Time
	Client    *http.Client
}

type FetchResult struct {
	AdapterID string
	SourceSys string
	FetchedAt time.Time
	RawSHA256 string
	Items     []RawObservation
}

type RawObservation struct {
	Kind       string
	SourceKey  string
	Subject    string
	Statement  string
	Detail     string
	Confidence float64
	Evidence   []string
}

type Fetcher interface {
	ID() string
	System() string
	Fetch(ctx context.Context, req FetchRequest) (FetchResult, error)
}
