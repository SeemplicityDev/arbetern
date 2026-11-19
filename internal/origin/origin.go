// Package origin names the entry path a turn or a pull request came from.
package origin

// Source is the entry path that produced a turn or opened a pull request.
type Source string

const (
	Slack     Source = "slack"
	Chat      Source = "chat"
	Workflow  Source = "workflow"
	Dashboard Source = "dashboard"
	Project   Source = "project"
	// Profile is the platform describing a person from their own turns, not an agent answering anyone.
	Profile Source = "profile"
)

// OrDefault returns Slack for an unset source: a slash command carries none.
func (s Source) OrDefault() Source {
	if s == "" {
		return Slack
	}
	return s
}
