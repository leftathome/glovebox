package strictjson

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode"
)

// foldKey must collide for every pair strings.EqualFold matches, and only
// for those: the duplicate check is then exactly the EqualFold check it
// replaces, in linear time. EqualFold is rune by rune, so checking every
// rune against every member of its case-fold orbit covers all strings.
func TestFoldKeyIsExactlyEqualFold(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogates are not valid in a string
		}
		s := string(r)
		k := foldKey(s)
		if !strings.EqualFold(s, k) {
			t.Fatalf("%U folds to %q, which EqualFold does not match", r, k)
		}
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if foldKey(string(f)) != k {
				t.Fatalf("%U and %U are EqualFold but fold to %q and %q", r, f, k, foldKey(string(f)))
			}
		}
	}
	// Different orbits never collide: the fold is a member of the orbit.
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("aAkKsSKſßẞθΘϑϴµΜμ�İıiI")
	for i := 0; i < 20000; i++ {
		a, b := randKey(rng, alphabet), randKey(rng, alphabet)
		if (foldKey(a) == foldKey(b)) != strings.EqualFold(a, b) {
			t.Fatalf("%q %q: fold %v, EqualFold %v", a, b, foldKey(a) == foldKey(b), strings.EqualFold(a, b))
		}
	}
	// Invalid UTF-8 decodes to U+FFFD for both.
	if foldKey("\xff") != foldKey("\xfe") || !strings.EqualFold("\xff", "\xfe") {
		t.Error("invalid bytes fold differently from EqualFold")
	}
}

func randKey(rng *rand.Rand, alphabet []rune) string {
	n := 1 + rng.Intn(3)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteRune(alphabet[rng.Intn(len(alphabet))])
	}
	return sb.String()
}

func TestDuplicateKeysUnderUnicodeFolding(t *testing.T) {
	for name, js := range map[string]string{
		"kelvin":       `{"k":1,"K":2}`,
		"long s":       `{"s":1,"ſ":2}`,
		"long s, word": `{"scopes":1,"ſcopes":2}`,
		"sharp s":      `{"ß":1,"ẞ":2}`,
		"theta":        `{"ϑ":1,"ϴ":2}`,
		"escaped":      `{"ab":1,"AB":2}`,
	} {
		var v map[string]int
		if err := Decode([]byte(js), &v); err == nil || !strings.Contains(err.Error(), "duplicate key") {
			t.Errorf("%s: %v", name, err)
		}
	}
	var v map[string]int
	if err := Decode([]byte(`{"k":1,"l":2,"ı":3,"i":4}`), &v); err != nil {
		t.Errorf("distinct keys refused: %v", err)
	}
}

func manyKeys(n int) []byte {
	var sb strings.Builder
	sb.WriteByte('{')
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"k%d":%d`, i, i)
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// 100k keys in one object are refused, fast, under a key cap.
func TestManyKeysAreRefusedFast(t *testing.T) {
	doc := manyKeys(100_000)
	start := time.Now()
	var v map[string]int
	err := DecodeLimits(doc, &v, Limits{MaxObjectKeys: 64, MaxTotalKeys: 1000})
	if !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("not refused: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("refusal took %s", el)
	}
}

// Without a cap the duplicate check is linear: 100k distinct keys pass in
// well under the time a pairwise comparison would take (minutes).
func TestDuplicateCheckIsLinear(t *testing.T) {
	doc := manyKeys(100_000)
	start := time.Now()
	var v map[string]int
	if err := Decode(doc, &v); err != nil || len(v) != 100_000 {
		t.Fatalf("%v, %d keys", err, len(v))
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("100k keys took %s", el)
	}
}

func TestKeyLimits(t *testing.T) {
	type in struct {
		A map[string]int   `json:"a"`
		L []map[string]int `json:"l"`
	}
	lim := Limits{MaxObjectKeys: 3, MaxTotalKeys: 7} // a, x, l and the list's keys
	for name, c := range map[string]struct {
		js string
		ok bool
	}{
		"at the object cap":        {`{"a":{"x":1,"y":2,"z":3}}`, true},
		"over the object cap":      {`{"a":{"w":0,"x":1,"y":2,"z":3}}`, false},
		"at the total cap":         {`{"a":{"x":1},"l":[{"a":1,"b":2},{"c":3,"d":4}]}`, true},
		"over the total cap":       {`{"a":{"x":1},"l":[{"a":1,"b":2},{"c":3,"d":4},{"e":5}]}`, false},
		"top level counts as well": {`{"a":{},"l":[],"b":1,"c":2}`, false},
	} {
		var v in
		err := DecodeLimits([]byte(c.js), &v, lim)
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok && !errors.Is(err, ErrTooManyKeys) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Zero means no cap.
	var v map[string]int
	if err := DecodeLimits(manyKeys(5000), &v, Limits{}); err != nil {
		t.Errorf("no cap: %v", err)
	}
}
