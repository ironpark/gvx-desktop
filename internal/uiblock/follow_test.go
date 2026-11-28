package uiblock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFollowCoalescingDiffAndGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	ch := make(chan struct{})
	cur := View{
		Tree:    &Node{Kind: KindText, Text: "v1"},
		Title:   "Main",
		State:   "running",
		Chrome:  "toolbar",
		Changed: ch,
	}
	viewFn := func() View {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	bump := func(fn func(v *View)) {
		mu.Lock()
		fn(&cur)
		close(ch)
		ch = make(chan struct{})
		cur.Changed = ch
		mu.Unlock()
	}

	updCh := make(chan Update, 16)
	sendFn := func(u Update) error {
		updCh <- u
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, 40*time.Millisecond, viewFn, sendFn)
	}()

	// 1. Initial full tree (Seq == 1).
	u1 := <-updCh
	if u1.Seq != 1 || u1.Tree == nil || u1.Tree.Text != "v1" || len(u1.Ops) != 0 || u1.Chrome != "toolbar" {
		t.Fatalf("u1 = %+v", u1)
	}

	// 2. Rapid burst of changes within coalesce window produces a single diff update.
	bump(func(v *View) { v.Tree = &Node{Kind: KindText, Text: "v2"} })
	time.Sleep(5 * time.Millisecond)
	bump(func(v *View) { v.Tree = &Node{Kind: KindText, Text: "v3"} })

	u2 := <-updCh
	if u2.Seq != 2 || u2.Tree != nil || len(u2.Ops) != 1 || u2.Ops[0].Op != OpUpdate || u2.Ops[0].Node.Text != "v3" {
		t.Fatalf("u2 = %+v", u2)
	}

	// 3. No-op signal (same tree pointer & metadata) is suppressed; metadata change emits update.
	bump(func(v *View) {})
	time.Sleep(50 * time.Millisecond)
	select {
	case extra := <-updCh:
		t.Fatalf("unexpected no-op update: %+v", extra)
	default:
	}

	bump(func(v *View) { v.Title = "Updated Title"; v.Stale = true })
	u3 := <-updCh
	if u3.Seq != 3 || u3.Title != "Updated Title" || !u3.Stale || u3.Tree != nil || len(u3.Ops) != 0 {
		t.Fatalf("u3 = %+v", u3)
	}

	// 4. Tree nil -> non-nil sends full tree.
	bump(func(v *View) { v.Tree = nil; v.Err = "boom" })
	u4 := <-updCh
	if u4.Seq != 4 || u4.Error != "boom" || u4.Tree != nil {
		t.Fatalf("u4 = %+v", u4)
	}
	bump(func(v *View) { v.Tree = &Node{Kind: KindText, Text: "recovered"}; v.Err = "" })
	u5 := <-updCh
	if u5.Seq != 5 || u5.Tree == nil || u5.Tree.Text != "recovered" {
		t.Fatalf("u5 = %+v", u5)
	}

	// 5. Gone emits final Gone=true update and returns ErrGone.
	bump(func(v *View) { v.Gone = true })
	u6 := <-updCh
	if u6.Seq != 6 || !u6.Gone {
		t.Fatalf("u6 = %+v", u6)
	}
	if err := <-done; !errors.Is(err, ErrGone) {
		t.Fatalf("Follow err = %v, want ErrGone", err)
	}
}

func TestFollowCancelAndSendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	var gotSeq uint64
	err := Follow(ctx, 0, func() View {
		return View{Tree: &Node{Kind: KindText, Text: "hi"}, Changed: ch}
	}, func(u Update) error {
		gotSeq = u.Seq
		return nil
	})
	if !errors.Is(err, context.Canceled) || gotSeq != 1 {
		t.Fatalf("cancel: err=%v seq=%d", err, gotSeq)
	}

	wantErr := errors.New("broken pipe")
	err = Follow(context.Background(), 10*time.Millisecond, func() View {
		return View{Tree: &Node{Kind: KindText, Text: "hi"}, Changed: ch}
	}, func(Update) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("send error: %v, want %v", err, wantErr)
	}
}
