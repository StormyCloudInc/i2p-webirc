package bot

import (
	"fmt"
	"testing"
	"time"
)

func TestHistoryNewestChronological(t *testing.T) {
	for _, count := range []int{3, 50, 65} {
		h := NewChannelHistory(50)
		for i := 0; i < count; i++ {
			h.Add(Message{Content: fmt.Sprint(i), Timestamp: time.Unix(int64(i), 0)})
		}
		msgs := h.GetRecent(10)
		want := min(count, 10)
		if len(msgs) != want {
			t.Fatal("wrong size")
		}
		for i, m := range msgs {
			if m.Content != fmt.Sprint(count-want+i) {
				t.Fatalf("count %d message %d=%s", count, i, m.Content)
			}
		}
		if len(h.GetRecent(0)) != 0 || len(h.GetRecent(-1)) != 0 {
			t.Fatal("nonpositive limit")
		}
	}
}
