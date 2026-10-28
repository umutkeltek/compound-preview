package steamweb

import "encoding/json"

type appDetailsResponse map[string]appDetailsEntry

type appDetailsEntry struct {
	Success bool            `json:"success"`
	Data    *appDetailsData `json:"data,omitempty"`
}

type appDetailsData struct {
	SteamAppid    int64           `json:"steam_appid"`
	Name          string          `json:"name"`
	Genres        []namedIDPair   `json:"genres"`
	Categories    []namedIDPair   `json:"categories"`
	Developers    []string        `json:"developers"`
	Publishers    []string        `json:"publishers"`
	IsFree        bool            `json:"is_free"`
	ReleaseDate   releaseDateWire `json:"release_date"`
	PriceOverview *priceWire      `json:"price_overview,omitempty"`
}

type namedIDPair struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type releaseDateWire struct {
	Date string `json:"date"`
}

type priceWire struct {
	Currency        string `json:"currency"`
	Initial         int    `json:"initial"`
	Final           int    `json:"final"`
	DiscountPercent int    `json:"discount_percent"`
}

func decodeAppDetails(body []byte) (appDetailsResponse, error) {
	var out appDetailsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}
