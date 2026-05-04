package commands

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	maxConversationTurns = 10
	conversationTTL      = 10 * time.Minute
	// maxConversations is a hard safety cap on distinct (channel, user)
	// pairs held in memory. The background sweeper handles steady-state
	// pruning; this cap protects against pathological growth between
	// sweeps.
	maxConversations = 8192
)

type ConversationMemory struct {
	mu    sync.Mutex
	convs map[string]*conversation
}

type conversation struct {
	turns     []turn
	updatedAt time.Time
}

type turn struct {
	User      string
	Assistant string
}

func NewConversationMemory() *ConversationMemory {
	return &ConversationMemory{
		convs: make(map[string]*conversation),
	}
}

func conversationKey(channelID, userID string) string {
	return channelID + ":" + userID
}

func (cm *ConversationMemory) AddUserMessage(channelID, userID, text string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	key := conversationKey(channelID, userID)
	conv, ok := cm.convs[key]
	if !ok || time.Since(conv.updatedAt) > conversationTTL {
		conv = &conversation{}
		cm.convs[key] = conv
	}

	conv.turns = append(conv.turns, turn{User: text})
	conv.updatedAt = time.Now()

	if len(conv.turns) > maxConversationTurns {
		conv.turns = conv.turns[len(conv.turns)-maxConversationTurns:]
	}
}

func (cm *ConversationMemory) SetAssistantResponse(channelID, userID, text string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	key := conversationKey(channelID, userID)
	conv, ok := cm.convs[key]
	if !ok || len(conv.turns) == 0 {
		return
	}

	last := &conv.turns[len(conv.turns)-1]
	last.Assistant = text
	conv.updatedAt = time.Now()
}

func (cm *ConversationMemory) GetHistory(channelID, userID string) string {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	key := conversationKey(channelID, userID)
	conv, ok := cm.convs[key]
	if !ok || time.Since(conv.updatedAt) > conversationTTL {
		return ""
	}

	if len(conv.turns) <= 1 {
		return ""
	}

	var sb strings.Builder
	for _, t := range conv.turns[:len(conv.turns)-1] {
		fmt.Fprintf(&sb, "User: %s\n", t.User)
		if t.Assistant != "" {
			fmt.Fprintf(&sb, "Assistant: %s\n", t.Assistant)
		}
	}
	return sb.String()
}

// StartGC launches a background goroutine that sweeps the in-memory
// conversation map every `interval`, dropping entries whose last update
// is older than conversationTTL. The goroutine exits when ctx is
// cancelled. Safe to call with a nil receiver.
func (cm *ConversationMemory) StartGC(ctx context.Context, interval time.Duration) {
	if cm == nil {
		return
	}
	if interval <= 0 {
		interval = conversationTTL
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cm.sweep()
			}
		}
	}()
}

func (cm *ConversationMemory) sweep() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if len(cm.convs) == 0 {
		return
	}
	cutoff := time.Now().Add(-conversationTTL)
	removed := 0
	for k, v := range cm.convs {
		if v.updatedAt.Before(cutoff) {
			delete(cm.convs, k)
			removed++
		}
	}
	// Hard cap safety net — if the map is still oversized after expiry
	// pruning, drop the oldest entries until we're back under the cap.
	if len(cm.convs) > maxConversations {
		over := len(cm.convs) - maxConversations
		type kv struct {
			key string
			ts  time.Time
		}
		all := make([]kv, 0, len(cm.convs))
		for k, v := range cm.convs {
			all = append(all, kv{k, v.updatedAt})
		}
		for i := 0; i < over && len(all) > 0; i++ {
			oldest := 0
			for j := 1; j < len(all); j++ {
				if all[j].ts.Before(all[oldest].ts) {
					oldest = j
				}
			}
			delete(cm.convs, all[oldest].key)
			all[oldest] = all[len(all)-1]
			all = all[:len(all)-1]
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[conversation-memory] gc: removed %d stale entries (remaining=%d)",
			removed, len(cm.convs))
	}
}
