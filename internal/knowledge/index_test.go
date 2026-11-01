package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDefaultSearchKeepsAnimeAndMangaCanonSeparate(t *testing.T) {
	index, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	animeJSON, err := index.Search(context.Background(), "кто такая Яку-нэко")
	if err != nil {
		t.Fatalf("Search(anime) error = %v", err)
	}
	animeFacts := decodeFacts(t, animeJSON)
	if len(animeFacts) == 0 || animeFacts[0].Canon != "anime" {
		t.Fatalf("Search(anime) facts = %#v, want anime canon ranked first", animeFacts)
	}

	mangaJSON, err := index.Search(context.Background(), "зависимость Яку в манге")
	if err != nil {
		t.Fatalf("Search(manga) error = %v", err)
	}
	mangaFacts := decodeFacts(t, mangaJSON)
	if len(mangaFacts) == 0 || mangaFacts[0].ID != "manga.yaku" || mangaFacts[0].Canon != "manga" || mangaFacts[0].SourceKind != "secondary_manga_reference" {
		t.Fatalf("Search(manga) facts = %#v, want sourced manga-only fact", mangaFacts)
	}
}

func TestDefaultSearchIncludesSeasonPlotAndNamedSideCharacters(t *testing.T) {
	index, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	for _, test := range []struct {
		query string
		id    string
	}{
		{query: "что происходит в 12 серии", id: "anime.season1.ep12"},
		{query: "Никотиновый демон", id: "anime.nicotine_demon"},
		{query: "Пенпен-нэко после тайфуна", id: "anime.penpen"},
		{query: "говорящий Король оленей", id: "anime.deer_king"},
	} {
		result, err := index.Search(context.Background(), test.query)
		if err != nil {
			t.Fatalf("Search(%q) error = %v", test.query, err)
		}
		facts := decodeFacts(t, result)
		if len(facts) == 0 || facts[0].ID != test.id {
			t.Errorf("Search(%q) first fact = %#v, want %q", test.query, facts, test.id)
		}
	}
}

func TestSearchReturnsEmptyForUnknownCanonAndRejectsInvalidQuery(t *testing.T) {
	index, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	result, err := index.Search(context.Background(), "факт которого нет в базе")
	if err != nil {
		t.Fatalf("Search(unknown) error = %v", err)
	}
	var response struct {
		Facts []json.RawMessage `json:"facts"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil || len(response.Facts) != 0 {
		t.Fatalf("Search(unknown) = (%s, %v), want no facts", result, err)
	}
	if _, err := index.Search(context.Background(), "  "); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("Search(blank) error = %v, want ErrInvalidQuery", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.Search(canceled, "Янико"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(canceled) error = %v, want context.Canceled", err)
	}
}

type searchFact struct {
	ID         string `json:"id"`
	Canon      string `json:"canon"`
	SourceKind string `json:"source_kind"`
}

func decodeFacts(t *testing.T, value string) []searchFact {
	t.Helper()
	var response struct {
		Facts []searchFact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(value), &response); err != nil {
		t.Fatalf("decode search result %s: %v", value, err)
	}
	return response.Facts
}
