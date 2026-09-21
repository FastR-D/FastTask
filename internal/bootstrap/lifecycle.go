package bootstrap

import "go.uber.org/fx"

// LifecycleObserver records component start/stop transitions. Production uses a
// no-op implementation; tests inject a spy to assert the shutdown ordering
// contract from wiring.md §6 ("停机时 HTTP 必须先于 Worker 停止") and §8.
//
// fx runs OnStart hooks in module declaration order and OnStop hooks in reverse.
// Because serveOptions appends HTTPModule last, HTTP starts last and stops
// first. The observer makes that ordering directly assertable instead of
// implied by option order alone.
type LifecycleObserver interface {
	Started(component string)
	Stopped(component string)
}

// LifecycleModule provides the default no-op observer. Tests omit it and
// provide a spy instead, so exactly one LifecycleObserver is ever in the graph.
var LifecycleModule = fx.Module("lifecycle",
	fx.Provide(func() LifecycleObserver { return nopObserver{} }),
)

type nopObserver struct{}

func (nopObserver) Started(string) {}
func (nopObserver) Stopped(string) {}
