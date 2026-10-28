package steamspy

import "encoding/json"

type estimate struct {
	AppID          string `json:"appid"`
	Name           string `json:"name"`
	Developer      string `json:"developer"`
	Publisher      string `json:"publisher"`
	Owners         string `json:"owners"`
	OwnersVariance string `json:"owners_variance"`
	PlayersForever string `json:"players_forever"`
	Players2Weeks  string `json:"players_2_weeks"`
	AverageForever string `json:"average_forever"`
	Average2Weeks  string `json:"average_2_weeks"`
	MedianForever  string `json:"median_forever"`
	Median2Weeks   string `json:"median_2_weeks"`
	CCU            string `json:"ccu"`
	Price          string `json:"price"`
	ScoreRank      string `json:"score_rank"`
	Positive       string `json:"positive"`
	Negative       string `json:"negative"`
	Tags           string `json:"tags"`
}

func decodeEstimate(body []byte) (estimate, error) {
	var e estimate
	if err := json.Unmarshal(body, &e); err != nil {
		return estimate{}, err
	}
	return e, nil
}
