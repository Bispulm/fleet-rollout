package rollout

import (
	"fmt"
	"regexp"
	"strconv"
)

var ratePattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]{1,2}))?%$`)

// ParseRate converts a percentage such as "2%" or "0.25%" to basis points. It
// accepts at most two decimals, the precision basis points can represent exactly.
func ParseRate(s string) (int, error) {
	match := ratePattern.FindStringSubmatch(s)
	if match == nil {
		return 0, fmt.Errorf("%q is not a percentage such as 2%% or 0.25%%", s)
	}
	whole, err := strconv.Atoi(match[1])
	if err != nil || whole > 100 {
		return 0, fmt.Errorf("%q is above 100%%", s)
	}
	fraction := 0
	if match[2] != "" {
		fraction, err = strconv.Atoi((match[2] + "0")[:2])
		if err != nil {
			return 0, fmt.Errorf("%q is not a percentage: %w", s, err)
		}
	}
	bps := whole*100 + fraction
	if bps > FullBPS {
		return 0, fmt.Errorf("%q is above 100%%", s)
	}
	return bps, nil
}
