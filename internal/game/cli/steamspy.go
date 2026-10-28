package cli

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"compound/internal/game/source/gamesource"
	"compound/internal/game/source/steamspy"
)

func observeSteamSpy(appids []int64, _, companies []string, homePath string, now time.Time) (gameObserveResult, error) {
	f := steamspy.New("", nil)
	req := gamesource.FetchRequest{
		AppIDs:    appids,
		Companies: companies,
		Now:       now,
		Client:    &http.Client{Timeout: 30 * time.Second},
	}
	res, err := f.Fetch(context.Background(), req)
	if err != nil {
		return gameObserveResult{}, fmt.Errorf("steamspy fetch: %w", err)
	}
	ar, err := gamesource.Apply(context.Background(), homePath, res, now)
	if err != nil {
		return gameObserveResult{AdapterID: ar.AdapterID, SourceSystem: ar.SourceSystem, Revision: ar.Revision, Written: ar.Written, Failed: ar.Failed}, err
	}
	return gameObserveResult{
		AdapterID:    ar.AdapterID,
		SourceSystem: ar.SourceSystem,
		Revision:     ar.Revision,
		Written:      ar.Written,
		Failed:       ar.Failed,
		CandidateIDs: ar.CandidateIDs,
		NoPromoteIDs: ar.NoPromoteIDs,
	}, nil
}

func runGameObserve(adapter string, appids []int64, genres, companies []string, homePath string, now time.Time) (gameObserveResult, error) {
	switch adapter {
	case "steamweb":
		return observeSteamweb(appids, genres, companies, homePath, now)
	case "steamspy":
		return observeSteamSpy(appids, nil, companies, homePath, now)
	default:
		return gameObserveResult{}, fmt.Errorf("unknown adapter %q (want steamweb|steamspy)", adapter)
	}
}
