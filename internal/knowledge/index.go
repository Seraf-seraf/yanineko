package knowledge

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxResults = 8
const maxResultBytes = 12 * 1024
const minimumResultScore = 4
const maxQueryLength = 1000
const maxQueryBytes = 4096

//go:embed canon.json
var corpus embed.FS

var ErrInvalidQuery = errors.New("invalid world-memory query")

type fact struct {
	ID               string   `json:"id"`
	Subject          string   `json:"subject"`
	Aliases          []string `json:"aliases"`
	Canon            string   `json:"canon"`
	Priority         int      `json:"priority"`
	Summary          string   `json:"summary"`
	Source           string   `json:"source"`
	SourceKind       string   `json:"source_kind"`
	Supplemental     string   `json:"supplemental_source,omitempty"`
	SupplementalKind string   `json:"supplemental_kind,omitempty"`
	AsOf             string   `json:"as_of,omitempty"`
}

type result struct {
	ID         string `json:"id"`
	Subject    string `json:"subject"`
	Canon      string `json:"canon"`
	Summary    string `json:"summary"`
	Source     string `json:"source"`
	SourceKind string `json:"source_kind"`
	AsOf       string `json:"as_of,omitempty"`
}

type document struct {
	SnapshotDate string `json:"snapshot_date"`
	Facts        []fact `json:"facts"`
}

type Index struct {
	items []fact
}

func Default() (*Index, error) {
	data, err := corpus.ReadFile("canon.json")
	if err != nil {
		return nil, errors.New("load bundled world memory")
	}
	var source document
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, errors.New("parse bundled world memory")
	}
	if len(source.Facts) == 0 {
		return nil, errors.New("bundled world memory is empty")
	}
	seen := make(map[string]struct{}, len(source.Facts))
	for _, item := range source.Facts {
		if item.ID == "" || item.Subject == "" || item.Summary == "" || item.Source == "" || (item.Canon != "anime" && item.Canon != "manga") {
			return nil, fmt.Errorf("invalid bundled world-memory fact %q", item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			return nil, fmt.Errorf("duplicate bundled world-memory fact %q", item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	return &Index{items: source.Facts}, nil
}

func (i *Index) Search(ctx context.Context, query string) (string, error) {
	if i == nil || len(i.items) == 0 || strings.TrimSpace(query) == "" || len(query) > maxQueryBytes || utf8.RuneCountInString(query) > maxQueryLength || strings.ContainsRune(query, '\x00') {
		return "", ErrInvalidQuery
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	queryText := normalize(query)
	queryTokens := tokens(queryText)
	if len(queryTokens) == 0 {
		return "", ErrInvalidQuery
	}
	type match struct {
		fact  fact
		score int
	}
	matches := make([]match, 0, len(i.items))
	for _, item := range i.items {
		score := relevance(queryText, queryTokens, item)
		if score >= minimumResultScore {
			matches = append(matches, match{fact: item, score: score})
		}
	}
	sort.SliceStable(matches, func(left int, right int) bool {
		if matches[left].score != matches[right].score {
			return matches[left].score > matches[right].score
		}
		if matches[left].fact.Priority != matches[right].fact.Priority {
			return matches[left].fact.Priority < matches[right].fact.Priority
		}
		return matches[left].fact.ID < matches[right].fact.ID
	})
	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}
	results := make([]result, 0, len(matches))
	for _, item := range matches {
		results = append(results, result{
			ID: item.fact.ID, Subject: item.fact.Subject, Canon: item.fact.Canon,
			Summary: item.fact.Summary, Source: item.fact.Source,
			SourceKind: item.fact.SourceKind, AsOf: item.fact.AsOf,
		})
	}
	if len(results) == 0 {
		return `{"query":` + quote(query) + `,"facts":[],"message":"В локальной базе точного факта нет."}`, nil
	}
	data, err := json.Marshal(struct {
		Query string   `json:"query"`
		Facts []result `json:"facts"`
	}{Query: query, Facts: results})
	if err != nil {
		return "", errors.New("encode world-memory results")
	}
	for len(results) > 1 && len(data) > maxResultBytes {
		results = results[:len(results)-1]
		data, err = json.Marshal(struct {
			Query string   `json:"query"`
			Facts []result `json:"facts"`
		}{Query: query, Facts: results})
		if err != nil {
			return "", errors.New("encode world-memory results")
		}
	}
	return string(data), nil
}

func relevance(query string, queryTokens []string, item fact) int {
	subject := normalize(item.Subject)
	summary := normalize(item.Summary)
	if query == subject {
		return 40
	}
	score := 0
	for _, alias := range item.Aliases {
		aliasText := normalize(alias)
		if aliasText == "" {
			continue
		}
		if query == aliasText {
			score += 32
		} else if containsPhrase(query, aliasText) {
			score += 20
		}
	}
	if containsPhrase(query, subject) || containsPhrase(subject, query) {
		score += 12
	}
	for _, token := range queryTokens {
		if isSearchStopWord(token) {
			continue
		}
		matchedAlias := false
		for _, alias := range item.Aliases {
			if containsToken(tokens(normalize(alias)), token) {
				score += 4
				matchedAlias = true
				break
			}
		}
		if matchedAlias {
			continue
		}
		if containsToken(tokens(subject), token) {
			score += 3
		} else if containsToken(tokens(summary), token) {
			score++
		}
	}
	return score
}

func containsPhrase(text string, phrase string) bool {
	return strings.Contains(" "+text+" ", " "+phrase+" ")
}

func containsToken(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func isSearchStopWord(token string) bool {
	if len([]rune(token)) < 2 {
		return true
	}
	switch token {
	case "а", "без", "бы", "в", "во", "где", "да", "для", "до", "её", "если", "же", "за", "и", "из", "или", "как", "к", "когда", "кто", "ли", "мне", "на", "над", "не", "нет", "но", "о", "об", "от", "почему", "по", "под", "при", "про", "с", "со", "так", "такая", "такой", "то", "у", "что", "эта", "это", "этот", "я":
		return true
	default:
		return false
	}
}

func normalize(value string) string {
	var output strings.Builder
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
		} else {
			output.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(output.String()), " ")
}

func tokens(value string) []string {
	return strings.Fields(value)
}

func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
