package commands

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/justmike1/arbetern/internal/safego"
	"github.com/justmike1/arbetern/llm"
)

// ModelTiers are the optional models an interactive turn may start on, and the router that picks one per request.
type ModelTiers struct {
	Light    *llm.Client
	Heavy    *llm.Client
	Router   *llm.ModelRouter
	SkipAcks bool
}

const maxAckRunes = 120

var errRouterPanicked = errors.New("model router panicked")

type routeDecision struct {
	tier llm.Tier
	took time.Duration
	err  error
}

func (d *routeDecision) routed() bool { return d != nil && d.err == nil }

func (d *routeDecision) is(t llm.Tier) bool { return d.routed() && d.tier == t }

type turnStart struct {
	client *llm.Client
	// codeClient is what the first code tool call moves the turn to; nil keeps it where it started.
	codeClient *llm.Client
}

func (h *GeneralHandler) startRouting(ctx context.Context, text string, ackable bool) func() *routeDecision {
	if !h.routingMatters(ackable) {
		return func() *routeDecision { return nil }
	}
	ch := make(chan *routeDecision, 1)
	safego.Go("model router: classify", func() {
		d := &routeDecision{err: errRouterPanicked}
		started := time.Now()
		defer func() {
			d.took = time.Since(started)
			ch <- d
		}()
		d.tier, d.err = h.tiers.Router.Route(ctx, text)
	})
	var (
		once sync.Once
		d    *routeDecision
	)
	return func() *routeDecision {
		once.Do(func() { d = <-ch })
		return d
	}
}

func (h *GeneralHandler) routingMatters(ackable bool) bool {
	if h.tiers.Router == nil || h.modelsClient == nil {
		return false
	}
	if ackable {
		return true
	}
	general := h.modelsClient.Model()
	for _, c := range []*llm.Client{h.tiers.Light, h.codeModelsClient, h.tiers.Heavy} {
		if c != nil && c.Model() != general {
			return true
		}
	}
	return false
}

func (h *GeneralHandler) chooseStart(d *routeDecision, codeIntent, thanksOnly bool) turnStart {
	general, code := h.modelsClient, h.codeModelsClient
	if code == nil {
		code = general
	}
	start := turnStart{client: general, codeClient: code}
	if !d.routed() {
		if codeIntent {
			start.client = code
		}
		return start
	}
	switch d.tier {
	case llm.TierThanks, llm.TierLight:
		// A follow-up such as "yes, do it" reads as trivial on its own but can
		// set off the heaviest work in the conversation.
		if h.tiers.Light != nil && (thanksOnly || !h.followUp) {
			start.client = h.tiers.Light
		}
	case llm.TierCode:
		start.client = code
	case llm.TierHeavy:
		if h.tiers.Heavy != nil {
			start = turnStart{client: h.tiers.Heavy}
		}
	}
	return start
}

func logTurnStart(prefix string, d *routeDecision, start turnStart, codeIntent bool) {
	switch {
	case d.routed():
		log.Printf("%s model router: %s request, starting on %s (routed in %s)", prefix, d.tier, start.client.Model(), d.took.Round(time.Millisecond))
		return
	case d != nil && !errors.Is(d.err, llm.ErrDependencyDown):
		log.Printf("%s model router failed after %s, using keyword routing: %v", prefix, d.took.Round(time.Millisecond), d.err)
	}
	if codeIntent {
		log.Printf("%s using code model (%s) for code-related request", prefix, start.client.Model())
	}
}

var thanksWords = setOf(
	"thanks", "thank", "thx", "thnx", "thanx", "ty", "tysm", "tyvm", "cheers", "appreciate", "appreciated",
	"grateful", "kudos", "bye", "goodbye", "cya", "danke", "merci", "gracias", "grazie", "obrigado", "obrigada",
	"תודה", "תודות", "תנקס", "ביי",
)

// Never add approvals (yes, ok, sure, lgtm, go): a reply dropped as thanks that was really a "yes" loses the request.
var thanksFiller = setOf(
	"you", "u", "so", "much", "very", "a", "lot", "lots", "many", "again", "all", "for", "now", "the", "your",
	"this", "that", "it", "s", "re", "is", "was", "really", "super", "big", "quick", "help", "great", "perfect",
	"awesome", "amazing", "nice", "cool", "excellent", "wonderful", "fantastic", "brilliant", "legend", "best",
	"guys", "team", "mate", "man", "buddy", "dude", "bro", "have", "good", "day", "weekend", "evening", "night",
	"exactly", "what", "i", "needed", "got", "beaucoup", "schön", "schon", "muchas", "mille",
	"רבה", "מאוד", "עזרת", "אחי", "אחלה", "מעולה", "אלוף", "אלופה", "ענק", "יפה", "על", "העזרה", "הכל", "לך", "לכם",
)

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func onlyThanks(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || utf8.RuneCountInString(t) > maxAckRunes || strings.ContainsAny(t, "?`<>👍👌✅✔☑🆗🚀") {
		return false
	}
	thanked := false
	for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch {
		case thanksWords[w]:
			thanked = true
		case !thanksFiller[w]:
			return false
		}
	}
	return thanked
}

var slackMarkupRe = regexp.MustCompile("<[^>]*>|```[\\s\\S]*?```|`[^`]*`")

func (h *GeneralHandler) botAskedInThread(channelID, threadTS string) bool {
	msgs, err := h.slackClient.FetchThreadReplies(channelID, threadTS, 100)
	if err != nil || len(msgs) >= 100 {
		return true
	}
	seenReply := false
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Timestamp == threadTS {
			break
		}
		if m.BotID == "" {
			if seenReply {
				break
			}
			seenReply = true
			continue
		}
		if strings.Contains(slackMarkupRe.ReplaceAllString(m.Text, ""), "?") {
			return true
		}
	}
	return false
}
