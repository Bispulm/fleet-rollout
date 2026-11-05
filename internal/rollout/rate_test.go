package rollout

import "testing"

func TestParseRateAtItsBoundaries(t *testing.T) {
	cases := map[string]struct {
		want    int
		wantErr bool
	}{
		"0%":      {want: 0},
		"0.01%":   {want: 1},
		"0.5%":    {want: 50},
		"2%":      {want: 200},
		"99.99%":  {want: 9999},
		"100%":    {want: 10000},
		"100.00%": {want: 10000},
		"100.01%": {wantErr: true},
		"0.001%":  {wantErr: true},
		"%":       {wantErr: true},
		"":        {wantErr: true},
		"2":       {wantErr: true},
		".5%":     {wantErr: true},
		"5.%":     {wantErr: true},
		"1e2%":    {wantErr: true},
		"+1%":     {wantErr: true},
	}
	for input, tc := range cases {
		got, err := ParseRate(input)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseRate(%q) error = %v, wantErr %v", input, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRate(%q) = %d, want %d", input, got, tc.want)
		}
	}
}
