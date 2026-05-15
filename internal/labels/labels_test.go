package labels

import (
	"reflect"
	"testing"
)

func TestHasMultienv(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   map[string]string
		want bool
	}{
		"nil":              {nil, false},
		"empty":            {map[string]string{}, false},
		"unrelated":        {map[string]string{"foo": "bar"}, false},
		"prefix only":      {map[string]string{"multienv.proxy.domain": "x"}, true},
		"mixed":            {map[string]string{"foo": "bar", "multienv.x": "1"}, true},
		"prefix as value":  {map[string]string{"x": "multienv.foo"}, false},
		"bare prefix word": {map[string]string{"multienv": "x"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := HasMultienv(tc.in); got != tc.want {
				t.Errorf("HasMultienv(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestForAccessory(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"multienv.proxy.domain":      "app.example.com",
		"multienv.proxy.port":        "3000",
		"multienv.postgres.dbname":   "myapp",
		"com.docker.compose.project": "myapp",
		"multienv.proxy":             "ignored", // bare prefix has no key
		"unrelated":                  "x",
	}
	got := ForAccessory(in, "proxy")
	want := map[string]string{
		"domain": "app.example.com",
		"port":   "3000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ForAccessory proxy = %v, want %v", got, want)
	}

	if got := ForAccessory(in, "absent"); got != nil {
		t.Errorf("ForAccessory absent = %v, want nil", got)
	}
}

func TestAccessories(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   map[string]string
		want []string
	}{
		"none": {
			in:   map[string]string{"foo": "bar"},
			want: nil,
		},
		"single": {
			in:   map[string]string{"multienv.proxy.domain": "app.example.com"},
			want: []string{"proxy"},
		},
		"multiple sorted unique": {
			in: map[string]string{
				"multienv.proxy.domain":   "x",
				"multienv.proxy.tls":      "true",
				"multienv.postgres.db":    "myapp",
				"multienv.postgres.user":  "admin",
			},
			want: []string{"postgres", "proxy"},
		},
		"bare accessory with no sub-key": {
			in:   map[string]string{"multienv.cache": "true"},
			want: []string{"cache"},
		},
		"empty trailing prefix is ignored": {
			in:   map[string]string{"multienv.": "x"},
			want: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Accessories(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Accessories(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
