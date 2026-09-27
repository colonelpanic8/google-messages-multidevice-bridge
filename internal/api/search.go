package api

import (
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// No limit preserves the original complete-snapshot response.
func searchOptions(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := 0
	if values, present := r.URL.Query()["limit"]; present {
		if len(values) != 1 {
			http.Error(w, "limit must be 1..500", 400)
			return "", 0, false
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || n > 500 {
			http.Error(w, "limit must be 1..500", 400)
			return "", 0, false
		}
		limit = n
	}
	return q, limit, true
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func searchMatch(q string, names []string, addresses []string) bool {
	if q == "" {
		return true
	}
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), q) {
			return true
		}
	}
	numeric := true
	for _, r := range q {
		if !unicode.IsDigit(r) && !strings.ContainsRune("+()- .", r) {
			numeric = false
			break
		}
	}
	number := digits(q)
	for _, address := range addresses {
		if strings.Contains(strings.ToLower(address), q) || (numeric && number != "" && strings.Contains(digits(address), number)) {
			return true
		}
	}
	return false
}
