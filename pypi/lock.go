package pypi

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const maxMinor = 40

var (
	specOperators = []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"}
	tomlEscaper   = strings.NewReplacer(`\`, `\\`, `"`, `\"`)
)

// LockLines renders a release the way uv.lock records it: its version, its sdist and the wheels that install on a Python version requiresPython admits.
func (r *Release) LockLines(requiresPython string, uploadTime bool) (lines string, skipped int, err error) {
	admits, err := pythonVersions(requiresPython)
	if err != nil {
		return "", 0, err
	}
	var sdist *File
	var wheels []File
	for i, f := range r.Files {
		switch {
		case f.Sdist:
			if sdist == nil {
				sdist = &r.Files[i]
			}
		case !strings.HasSuffix(f.Filename, ".whl"):
		case wheelAdmitted(f.Filename, admits):
			wheels = append(wheels, f)
		default:
			skipped++
		}
	}
	slices.SortFunc(wheels, func(a, b File) int { return strings.Compare(a.Filename, b.Filename) })
	var b strings.Builder
	fmt.Fprintf(&b, "version = %s\n", tomlString(r.Version))
	if sdist != nil {
		fmt.Fprintf(&b, "sdist = %s\n", lockFile(*sdist, uploadTime))
	}
	if len(wheels) > 0 {
		b.WriteString("wheels = [\n")
		for _, w := range wheels {
			fmt.Fprintf(&b, "    %s,\n", lockFile(w, uploadTime))
		}
		b.WriteString("]\n")
	}
	return b.String(), skipped, nil
}

func lockFile(f File, uploadTime bool) string {
	s := fmt.Sprintf("{ url = %s, hash = %s, size = %d", tomlString(f.URL), tomlString("sha256:"+f.SHA256), f.Size)
	if uploadTime && !f.UploadedAt.IsZero() {
		s += ", upload-time = " + tomlString(f.UploadedAt.UTC().Format("2006-01-02T15:04:05.999Z07:00"))
	}
	return s + " }"
}

func tomlString(s string) string { return `"` + tomlEscaper.Replace(s) + `"` }

// CheckRequiresPython reports whether LockLines can read a requires-python specifier.
func CheckRequiresPython(spec string) error {
	_, err := parseSpecifier(spec)
	return err
}

type clause struct {
	op       string
	release  []int
	wildcard bool
}

func parseSpecifier(spec string) ([]clause, error) {
	bad := fmt.Errorf("requires_python %q is not a specifier such as >=3.10 or ==3.12.*", spec)
	var out []clause
	for _, part := range strings.Split(spec, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		i := slices.IndexFunc(specOperators, func(op string) bool { return strings.HasPrefix(part, op) })
		if i < 0 {
			return nil, bad
		}
		c := clause{op: specOperators[i]}
		v := strings.TrimSpace(part[len(c.op):])
		if c.wildcard = strings.HasSuffix(v, ".*"); c.wildcard && c.op != "==" && c.op != "!=" {
			return nil, bad
		}
		for _, n := range strings.Split(strings.TrimSuffix(v, ".*"), ".") {
			d, err := strconv.Atoi(n)
			if err != nil || d < 0 {
				return nil, bad
			}
			c.release = append(c.release, d)
		}
		if c.op == "~=" && len(c.release) < 2 {
			return nil, bad
		}
		out = append(out, c)
	}
	return out, nil
}

func segment(v []int, i int) int {
	if i < len(v) {
		return v[i]
	}
	return 0
}

func compareRelease(a, b []int) int {
	for i := range max(len(a), len(b)) {
		if c := cmp.Compare(segment(a, i), segment(b, i)); c != 0 {
			return c
		}
	}
	return 0
}

func hasPrefix(v, prefix []int) bool {
	for i, p := range prefix {
		if segment(v, i) != p {
			return false
		}
	}
	return true
}

func (c clause) admits(v []int) bool {
	switch c.op {
	case "==", "===":
		return (c.wildcard && hasPrefix(v, c.release)) || (!c.wildcard && compareRelease(v, c.release) == 0)
	case "!=":
		return (c.wildcard && !hasPrefix(v, c.release)) || (!c.wildcard && compareRelease(v, c.release) != 0)
	case "~=":
		return compareRelease(v, c.release) >= 0 && hasPrefix(v, c.release[:len(c.release)-1])
	case ">=":
		return compareRelease(v, c.release) >= 0
	case "<=":
		return compareRelease(v, c.release) <= 0
	case ">":
		return compareRelease(v, c.release) > 0
	case "<":
		return compareRelease(v, c.release) < 0
	}
	return false
}

// A minor version counts when any of its patch releases is admitted.
func pythonVersions(spec string) (func(major, minor int) bool, error) {
	clauses, err := parseSpecifier(spec)
	if err != nil {
		return nil, err
	}
	return func(major, minor int) bool {
		patches := []int{0, 999}
		for _, c := range clauses {
			if len(c.release) > 2 && c.release[0] == major && c.release[1] == minor {
				patches = append(patches, c.release[2]-1, c.release[2], c.release[2]+1)
			}
		}
		return slices.ContainsFunc(patches, func(patch int) bool {
			v := []int{major, minor, patch}
			return !slices.ContainsFunc(clauses, func(c clause) bool { return !c.admits(v) })
		})
	}, nil
}

// A tag it cannot read keeps the wheel.
func wheelAdmitted(filename string, admits func(major, minor int) bool) bool {
	parts := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
	if len(parts) < 5 {
		return true
	}
	abi := parts[len(parts)-2]
	return slices.ContainsFunc(strings.Split(parts[len(parts)-3], "."), func(tag string) bool { return tagAdmitted(tag, abi, admits) })
}

// pyXY and abi3 wheels install on their version and every later one, the rest on their version only.
func tagAdmitted(tag, abi string, admits func(major, minor int) bool) bool {
	i := strings.IndexFunc(tag, unicode.IsDigit)
	if i <= 0 {
		return true
	}
	major, from, to := int(tag[i]-'0'), 0, maxMinor
	if len(tag) > i+1 {
		minor, err := strconv.Atoi(tag[i+1:])
		if err != nil {
			return true
		}
		from = minor
		if tag[:i] != "py" && abi != "abi3" {
			to = minor
		}
	}
	for m := from; m <= to; m++ {
		if admits(major, m) {
			return true
		}
	}
	return false
}
