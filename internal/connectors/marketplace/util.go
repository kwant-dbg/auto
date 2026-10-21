package marketplace

import (
	"encoding/json"
	"sort"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func sortStrings(s []string) { sort.Strings(s) }
