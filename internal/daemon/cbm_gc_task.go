package daemon

import (
	"context"
	"log"
	"time"

	"github.com/carsteneu/yesmem/internal/codescan"
)

// cbmGCInterval: how often the daemon GC task runs.
const cbmGCInterval = 24 * time.Hour

// cbmGCDelay: first GC run after daemon boot. Delayed so an operator seeing
// a problem at boot has a window to disable the task before the first real
// deletion happens (files must still be >24h old to be eligible).
const cbmGCDelay = time.Hour

// startCBMGCTask GCs orphaned CBM worktree-index DBs once a day. Every
// deletion is logged by codescan.RunCBMGC; main-repo DBs are never touched.
func startCBMGCTask(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cbmGCDelay):
		}
		runCBMGCTaskOnce()
		ticker := time.NewTicker(cbmGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCBMGCTaskOnce()
			}
		}
	}()
}

func runCBMGCTaskOnce() {
	findings, err := codescan.RunCBMGC(false)
	if err != nil {
		log.Printf("cbm-gc: task: %v", err)
		return
	}
	var total int64
	for _, f := range findings {
		total += f.SizeBytes
	}
	log.Printf("cbm-gc: task: %d items, %s freed", len(findings), codescan.FormatCBMByteSize(total))
}
