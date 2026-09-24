// Package releasecontract defines the network-free contract for fork releases
// of lwying/sub2api.
//
// It answers two questions with plain data in and plain data out:
//
//   - may this version input become a fork release? (three-segment numeric form,
//     strictly above the fork's own published baseline, and not a version that
//     merely arrived with an upstream sync)
//   - does a release with this asset list offer a binary a user can install on a
//     given platform? (a platform archive plus checksums.txt; an image-only
//     release is never installable)
//
// Nothing here performs network, git or GitHub access. The release workflow
// hands it the fork's published versions and the release asset list, and tests
// hand it local fixtures, so the same decision is reproducible offline.
package releasecontract

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a fork release version in the required three-segment numeric form.
//
// Prerelease suffixes, build metadata, two-segment forms and a leading zero in
// any segment are rejected on purpose: the fork's in-app update path compares
// numeric segments only, so "v0.2.7-rc.1" would compare as older than v0.2.7.
type Version struct {
	Major int
	Minor int
	Patch int
}

// ParseVersion accepts "X.Y.Z" or "vX.Y.Z".
func ParseVersion(raw string) (Version, error) {
	return parse(raw, false)
}

// ParseTag accepts exactly "vX.Y.Z": the release entry points take tags, and a
// bare "1.2.3" input must not be treated as a tag.
func ParseTag(raw string) (Version, error) {
	return parse(raw, true)
}

func parse(raw string, requireVPrefix bool) (Version, error) {
	s := strings.TrimSpace(raw)
	if requireVPrefix {
		if !strings.HasPrefix(s, "v") {
			return Version{}, fmt.Errorf("version %q must be a v-prefixed tag like v1.2.3", raw)
		}
	}
	body := strings.TrimPrefix(s, "v")
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q must be exactly three numeric segments (X.Y.Z)", raw)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := parseSegment(p, raw)
		if err != nil {
			return Version{}, err
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, nil
}

func parseSegment(p, raw string) (int, error) {
	if p == "" {
		return 0, fmt.Errorf("version %q has an empty segment", raw)
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("version %q segment %q is not numeric", raw, p)
		}
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, fmt.Errorf("version %q segment %q has a leading zero", raw, p)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0, fmt.Errorf("version %q segment %q is not a number: %w", raw, p, err)
	}
	return n, nil
}

// String renders the canonical "X.Y.Z" form.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Tag renders the canonical "vX.Y.Z" form.
func (v Version) Tag() string { return "v" + v.String() }

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return sign(v.Major - o.Major)
	case v.Minor != o.Minor:
		return sign(v.Minor - o.Minor)
	case v.Patch != o.Patch:
		return sign(v.Patch - o.Patch)
	default:
		return 0
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// highestVersion returns the greatest parseable version of the given list.
// Unparseable entries are reported through invalid so callers can stay loud
// instead of silently treating garbage as a baseline.
func highestVersion(raws []string, invalid *[]string) (Version, bool) {
	var best Version
	found := false
	for _, raw := range raws {
		v, err := ParseVersion(raw)
		if err != nil {
			if invalid != nil {
				*invalid = append(*invalid, raw)
			}
			continue
		}
		if !found || v.Compare(best) > 0 {
			best = v
			found = true
		}
	}
	return best, found
}
