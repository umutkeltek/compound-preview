package cli

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"compound/internal/game/source/gamesource"
	"compound/internal/game/source/steamweb"
)

func observeSteamweb(appids []int64, genres, companies []string, homePath string, now time.Time) (gameObserveResult, error) {
	f := steamweb.New("", nil)
	req := gamesource.FetchRequest{
		AppIDs:    appids,
		Genres:    genres,
		Companies: companies,
		Now:       now,
		Client:    &http.Client{Timeout: 30 * time.Second},
	}
	res, err := f.Fetch(context.Background(), req)
	if err != nil {
		return gameObserveResult{}, fmt.Errorf("steamweb fetch: %w", err)
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
