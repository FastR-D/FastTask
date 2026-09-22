package bootstrap

import (
	"sort"
	"testing"

	"github.com/FastR-D/FastTask/internal/application"
	"go.uber.org/fx"
)

// TestJobValueGroupsCoverTheBuiltinJobTypes closes the gap doc/README.md §3.2 named: the fx value
// groups and application.BuiltinJobHandlers/BuiltinJobMaterializers are two lists of the same
// thing, and nothing used to assert they agree. A job type present in one and missing from the
// other is a production bug that no other test can see — the builtin list is what a unit test
// builds a Worker from, and the value group is what serve actually runs.
func TestJobValueGroupsCoverTheBuiltinJobTypes(t *testing.T) {
	type captured struct {
		fx.In
		Handlers    []application.JobHandler      `group:"job_handlers"`
		Materialize []application.JobMaterializer `group:"job_materializers"`
	}
	var got captured
	app := fx.New(
		fx.Supply(testConfig(t)),
		Core,
		WorkerModule,
		fx.Invoke(func(c captured) { got = c }),
		fx.NopLogger,
	)
	if err := app.Err(); err != nil {
		t.Fatalf("assemble the worker graph: %v", err)
	}

	handlerTypes := jobTypesOf(got.Handlers)
	builtinHandlerTypes := builtinJobTypesOf(application.BuiltinJobHandlers())
	assertSameTypes(t, "job_handlers value group", handlerTypes, builtinHandlerTypes)

	materializerTypes := materializerTypesOf(got.Materialize)
	builtinMaterializerTypes := builtinMaterializerTypesOf(application.BuiltinJobMaterializers())
	assertSameTypes(t, "job_materializers value group", materializerTypes, builtinMaterializerTypes)
}

func jobTypesOf(handlers []application.JobHandler) []string {
	var types []string
	for _, handler := range handlers {
		types = append(types, handler.JobTypes()...)
	}
	return types
}

func builtinJobTypesOf(handlers []application.JobHandler) []string { return jobTypesOf(handlers) }

func materializerTypesOf(materializers []application.JobMaterializer) []string {
	var types []string
	for _, materializer := range materializers {
		types = append(types, materializer.JobTypes()...)
	}
	return types
}

func builtinMaterializerTypesOf(materializers []application.JobMaterializer) []string {
	return materializerTypesOf(materializers)
}

func assertSameTypes(t *testing.T, name string, fromFX, fromBuiltins []string) {
	t.Helper()
	if len(fromFX) == 0 {
		t.Fatalf("%s is empty", name)
	}
	group := sortedUnique(fromFX)
	builtins := sortedUnique(fromBuiltins)
	if len(group) != len(builtins) {
		t.Fatalf("%s covers %v, the builtin list covers %v", name, group, builtins)
	}
	for i := range group {
		if group[i] != builtins[i] {
			t.Fatalf("%s covers %v, the builtin list covers %v", name, group, builtins)
		}
	}
	if len(group) != len(fromFX) {
		t.Fatalf("%s registers a job type twice: %v", name, fromFX)
	}
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
