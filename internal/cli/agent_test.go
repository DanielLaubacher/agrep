package cli

import (
	"reflect"
	"testing"
)

func TestSuggestVariants(t *testing.T) {
	cases := []struct {
		pattern string
		want    []string
	}{
		{"ConnectTimeout", []string{"connecttimeout", "connect", "timeout"}},
		{"retry_backoff_ms", []string{"retry", "backoff"}}, // "ms" too short
		{"epoll", nil}, // lowercase single word: no variants beyond itself
		{"HTTPServer", []string{"httpserver", "httpserver"}[0:1]},
	}
	for _, tc := range cases {
		var got []string
		for _, v := range suggestVariants(tc.pattern) {
			got = append(got, v.pattern)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("suggestVariants(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestAppendJSONString(t *testing.T) {
	got := string(appendJSONString(nil, "a\"b\\c\nd"))
	want := `"a\"b\\c\u000ad"`
	if got != want {
		t.Errorf("appendJSONString = %s, want %s", got, want)
	}
}
