// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Scenario is one end-to-end CLI flow. Each runs on a fresh server.
type Scenario struct {
	Name string
	// Needs returns why the CLI cannot run this scenario, or "".
	Needs func(c *bufCLI) string
	Run   func(t T, e *Env)
}

// scenarios is the conformance matrix's columns, in report order.
var scenarios = []Scenario{
	{Name: "login", Run: scenarioLogin},
	{Name: "module", Needs: needs("registry", "module", "create"), Run: scenarioModule},
	{Name: "push", Run: scenarioPush},
	{Name: "labels", Needs: needsLabels, Run: scenarioLabels},
	{Name: "dep-update", Run: scenarioDepUpdate},
	{Name: "build", Run: scenarioBuild},
	{Name: "export", Run: scenarioExport},
	{Name: "lint", Run: scenarioLint},
	{Name: "breaking", Run: scenarioBreaking},
	{Name: "generate", Run: scenarioGenerate},
}

func needs(path ...string) func(c *bufCLI) string {
	return func(c *bufCLI) string {
		if !c.has(path...) {
			return "no `buf " + strings.Join(path, " ") + "` command"
		}
		return ""
	}
}

func labelCmd(c *bufCLI, sub string) []string {
	return c.first([]string{"registry", "module", "label", sub}, []string{"registry", "label", sub})
}

func needsLabels(c *bufCLI) string {
	if labelCmd(c, "list") == nil || labelCmd(c, "archive") == nil {
		return "no `buf registry [module] label` commands"
	}
	return ""
}

// Fixture protos. weather is the base module; app imports it.
const (
	weatherPath  = "acme/weather/v1/weather.proto"
	weatherProto = `syntax = "proto3";

package acme.weather.v1;

// Forecast is a weather forecast.
message Forecast {
  string city = 1;
  double temperature = 2;
}
`
	appPath  = "acme/app/v1/app.proto"
	appProto = `syntax = "proto3";

package acme.app.v1;

import "acme/weather/v1/weather.proto";

// Trip is a planned trip.
message Trip {
  acme.weather.v1.Forecast forecast = 1;
}
`
)

// pushWeather creates and pushes acme/weather (to the default label, then
// to labels, if any: `push --label` only sets the labels it names) and
// returns its commit id and workspace.
func pushWeather(e *Env, labels ...string) (string, string) {
	e.t.Helper()
	e.CreateModule("weather")
	dir := e.Workspace("weather", nil, map[string]string{weatherPath: weatherProto, "LICENSE": "Apache-2.0\n", "README.md": "# Weather\n"})
	id := e.Push(dir)
	for _, l := range labels {
		if got := e.Push(dir, "--label="+l); got != id {
			e.t.Fatalf("push --label=%s = %s, want %s", l, got, id)
		}
	}
	return id, dir
}

// appWorkspace writes acme/app depending on acme/weather and runs dep update.
func appWorkspace(e *Env) string {
	dir := e.Workspace("app", []string{e.Mod("weather")}, map[string]string{appPath: appProto})
	e.OK(dir, "dep", "update")
	return dir
}

func scenarioLogin(t T, e *Env) {
	e.CreateModule("weather")
	dir := e.Workspace("weather", nil, map[string]string{weatherPath: weatherProto})
	e.NoToken = true

	// Without credentials a push is refused.
	errOut := e.Fails(dir, "push")
	t.Logf("unauthenticated push: %s", errOut)

	args := []string{"registry", "login", e.Host, "--token-stdin"}
	// Older CLIs (1.32) also prompt for a user name unless it is given.
	if strings.Contains(e.cli.helpText("registry", "login"), "--username") {
		args = append(args, "--username", "admin")
	}
	r := e.Exec("", e.Token+"\n", args...)
	if r.Err != nil {
		t.Fatalf("buf %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Err, r.Stdout, r.Stderr)
	}
	netrc := e.ReadFile(e.home, ".netrc")
	host, _, _ := strings.Cut(e.Host, ":")
	mustContain(t, ".netrc", netrc, host)

	if e.cli.has("registry", "whoami") {
		out := e.OK("", "registry", "whoami", e.Host)
		mustContain(t, "whoami", out, "admin")
	}
	// The stored credentials authenticate a push.
	e.Push(dir)

	e.OK("", "registry", "logout", e.Host)
	e.Fails(dir, "push")
}

func scenarioModule(t T, e *Env) {
	mod := e.Mod("weather")
	e.CreateModule("weather")
	if m := e.Module("weather"); m.Visibility != "public" || m.DefaultLabel != "main" {
		t.Fatalf("stored module = %+v", m)
	}
	var info map[string]any
	out := e.OK("", "registry", "module", "info", mod, "--format=json")
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("module info: %v\n%s", err, out)
	}
	if info["name"] != "weather" || info["owner"] != "acme" {
		t.Fatalf("module info = %v", info)
	}
	errOut := e.Fails("", "registry", "module", "create", mod)
	mustContain(t, "duplicate create", errOut, "already exists")
	e.Fails("", "registry", "module", "info", e.Mod("nope"))

	update := e.cli.first([]string{"registry", "module", "settings", "update"}, []string{"registry", "module", "update"})
	if update != nil {
		e.OK("", append(update, mod, "--description=forecasts", "--visibility=private")...)
		if m := e.Module("weather"); m.Description != "forecasts" || m.Visibility != "private" {
			t.Fatalf("stored module after update = %+v", m)
		}
	}
	e.OK("", "registry", "module", "deprecate", mod)
	if m := e.Module("weather"); !m.Deprecated {
		t.Fatalf("module not deprecated: %+v", m)
	}
	e.OK("", "registry", "module", "undeprecate", mod)
	e.OK("", "registry", "module", "delete", mod, "--force")
	e.Fails("", "registry", "module", "info", mod)
}

func scenarioPush(t T, e *Env) {
	first, dir := pushWeather(e)
	c := e.LabelCommit("weather", "main")
	if c.ID != first || !strings.HasPrefix(c.Digest, "b5:") {
		t.Fatalf("main = %+v, want commit %s with a b5 digest", c, first)
	}
	if again := e.Push(dir); again != first {
		t.Fatalf("re-push = %s, want %s (idempotent)", again, first)
	}
	if labeled := e.Push(dir, "--label=v1"); labeled != first {
		t.Fatalf("push --label = %s, want %s", labeled, first)
	}
	e.WriteFiles(dir, map[string]string{weatherPath: weatherProto + "\n// Extra is new.\nmessage Extra {}\n"})
	second := e.Push(dir)
	if second == first {
		t.Fatalf("changed content pushed to the same commit %s", first)
	}
	if c := e.LabelCommit("weather", "v1"); c.ID != first {
		t.Fatalf("v1 moved to %s", c.ID)
	}
	if e.cli.has("registry", "module", "commit", "info") || e.cli.has("registry", "commit", "info") {
		info := e.cli.first([]string{"registry", "module", "commit", "info"}, []string{"registry", "commit", "info"})
		out := e.OK("", append(info, e.Mod("weather")+":"+second, "--format=json")...)
		mustContain(t, "commit info", out, second)
	}
	// A module that does not exist is not created implicitly.
	missing := e.Workspace("missing", nil, map[string]string{"acme/missing/v1/m.proto": "syntax = \"proto3\";\npackage acme.missing.v1;\n"})
	e.Fails(missing, "push")
}

func scenarioLabels(t T, e *Env) {
	mod := e.Mod("weather")
	id, _ := pushWeather(e, "v1")
	out := e.OK("", append(labelCmd(e.cli, "list"), mod)...)
	mustContain(t, "label list", out, mod+":main", mod+":v1")
	if info := labelCmd(e.cli, "info"); info != nil {
		out := e.OK("", append(info, mod+":v1", "--format=json")...)
		mustContain(t, "label info", out, `"v1"`, id)
	}
	e.OK("", append(labelCmd(e.cli, "archive"), mod+":v1")...)
	out = e.OK("", append(labelCmd(e.cli, "list"), mod)...)
	mustNotContain(t, "label list after archive", out, mod+":v1")
	if strings.Contains(e.cli.helpText(labelCmd(e.cli, "list")...), "--archive-status") {
		out = e.OK("", append(labelCmd(e.cli, "list"), mod, "--archive-status=archived")...)
		mustContain(t, "archived labels", out, mod+":v1")
	}
	// The default label cannot be archived.
	e.Fails("", append(labelCmd(e.cli, "archive"), mod+":main")...)
	e.OK("", append(labelCmd(e.cli, "unarchive"), mod+":v1")...)
	out = e.OK("", append(labelCmd(e.cli, "list"), mod)...)
	mustContain(t, "label list after unarchive", out, mod+":v1")
}

func scenarioDepUpdate(t T, e *Env) {
	first, wdir := pushWeather(e)
	dir := appWorkspace(e)
	lock := e.ReadFile(dir, "buf.lock")
	c := e.LabelCommit("weather", "main")
	mustContain(t, "buf.lock", lock, e.Mod("weather"), first, c.Digest)

	// A new weather commit is picked up by the next dep update.
	e.WriteFiles(wdir, map[string]string{weatherPath: weatherProto + "\n// Extra is new.\nmessage Extra {}\n"})
	second := e.Push(wdir)
	e.OK(dir, "dep", "update")
	lock = e.ReadFile(dir, "buf.lock")
	mustContain(t, "buf.lock after update", lock, second, e.LabelCommit("weather", "main").Digest)
	mustNotContain(t, "buf.lock after update", lock, first)

	// The locked dependency builds from a cold cache (download + verify).
	e.ClearCache()
	e.OK(dir, "build")
	// Push records the dependency; the app builds as a remote module.
	e.CreateModule("app")
	e.Push(dir)
	img := e.OK("", "build", e.Mod("app"), "-o", "-#format=json")
	mustContain(t, "remote app image", img, appPath, weatherPath)
}

func scenarioBuild(t T, e *Env) {
	mod := e.Mod("weather")
	id, _ := pushWeather(e, "v1")
	var first string
	for _, ref := range []string{mod, mod + ":main", mod + ":v1", mod + ":" + id} {
		img := e.OK("", "build", ref, "-o", "-#format=json")
		mustContain(t, "build "+ref, img, weatherPath, "Forecast")
		if first == "" {
			first = img
		} else if img != first {
			t.Fatalf("build %s differs from build %s", ref, mod)
		}
	}
	e.Fails("", "build", mod+":nope")
	e.Fails("", "build", e.Mod("missing"))

	// A local workspace with a remote dependency builds from a cold cache.
	dir := appWorkspace(e)
	e.ClearCache()
	img := e.OK(dir, "build", "-o", "-#format=json")
	mustContain(t, "local build", img, appPath)
}

func scenarioExport(t T, e *Env) {
	mod := e.Mod("weather")
	id, _ := pushWeather(e)
	for _, ref := range []string{mod, mod + ":" + id} {
		out := filepath.Join(t.TempDir(), "export")
		e.OK("", "export", ref, "-o", out)
		if got := e.ReadFile(out, weatherPath); got != weatherProto {
			t.Fatalf("export %s: %s = %q", ref, weatherPath, got)
		}
	}
	// Exporting a dependent module includes its dependency's files.
	dir := appWorkspace(e)
	e.CreateModule("app")
	e.Push(dir)
	out := filepath.Join(t.TempDir(), "export")
	e.OK("", "export", e.Mod("app"), "-o", out)
	e.ReadFile(out, appPath)
	e.ReadFile(out, weatherPath)
	e.Fails("", "export", e.Mod("missing"), "-o", t.TempDir())
}

// legacyProto breaks the STANDARD lint rules (message name casing).
const legacyProto = `syntax = "proto3";

package acme.legacy.v1;

// old_reading is not PascalCase.
message old_reading {
  string value = 1;
}
`

func scenarioLint(t T, e *Env) {
	e.CreateModule("legacy")
	ldir := e.Workspace("legacy", nil, map[string]string{"acme/legacy/v1/legacy.proto": legacyProto})
	// Push does not lint; the remote module carries the violation.
	e.Push(ldir)

	// A clean local module importing the remote one lints clean: lint does
	// not check dependencies.
	dir := e.Workspace("app", []string{e.Mod("legacy")}, map[string]string{appPath: `syntax = "proto3";

package acme.app.v1;

import "acme/legacy/v1/legacy.proto";

// Trip is a planned trip.
message Trip {
  acme.legacy.v1.old_reading reading = 1;
}
`})
	e.OK(dir, "dep", "update")
	e.ClearCache()
	e.OK(dir, "lint")

	// A local violation is reported.
	e.WriteFiles(dir, map[string]string{"acme/app/v1/bad.proto": "syntax = \"proto3\";\n\npackage acme.app.v1;\n\n// bad_name is not PascalCase.\nmessage bad_name {}\n"})
	r := e.Exec(dir, "", "lint")
	if r.Err == nil {
		t.Fatalf("lint passed with a local violation")
	}
	mustContain(t, "lint output", r.Stdout+r.Stderr, "bad.proto", "PascalCase")
	mustNotContain(t, "lint output", r.Stdout+r.Stderr, "legacy.proto")

	// Linting the remote module as an input reports its violation.
	r = e.Exec("", "", "lint", e.Mod("legacy"))
	if r.Err == nil {
		t.Fatalf("lint of %s passed", e.Mod("legacy"))
	}
	mustContain(t, "remote lint output", r.Stdout+r.Stderr, "legacy.proto", "PascalCase")
}

func scenarioBreaking(t T, e *Env) {
	_, dir := pushWeather(e)
	e.OK(dir, "breaking", "--against", e.Mod("weather"))
	e.WriteFiles(dir, map[string]string{weatherPath: strings.Replace(weatherProto, "  double temperature = 2;\n", "", 1)})
	r := e.Exec(dir, "", "breaking", "--against", e.Mod("weather"))
	if r.Err == nil {
		t.Fatalf("breaking passed after deleting a field")
	}
	mustContain(t, "breaking output", r.Stdout+r.Stderr, "temperature")
}

func scenarioGenerate(t T, e *Env) {
	_, _ = pushWeather(e)
	dir := appWorkspace(e)
	gen := "version: v2\nplugins:\n  - local: " + plugin(t) + "\n    out: gen\n    opt: conformance\n"
	e.WriteFiles(dir, map[string]string{"buf.gen.yaml": gen})

	// Local module with a remote dependency: only local files by default.
	e.OK(dir, "generate")
	out := e.ReadFile(dir, filepath.Join("gen", appPath+".txt"))
	mustContain(t, "generated app", out, "parameter: conformance", "package: acme.app.v1", "message: Trip", "import: "+weatherPath)
	if _, err := os.Stat(filepath.Join(dir, "gen", weatherPath+".txt")); err == nil { //nolint:gosec // test temp dir
		t.Fatalf("dependency generated without --include-imports")
	}
	// --include-imports generates the remote dependency too.
	e.OK(dir, "generate", "--include-imports")
	out = e.ReadFile(dir, filepath.Join("gen", weatherPath+".txt"))
	mustContain(t, "generated weather", out, "package: acme.weather.v1", "message: Forecast")

	// A remote module as the input.
	rdir := t.TempDir()
	e.WriteFiles(rdir, map[string]string{"buf.gen.yaml": gen})
	e.OK(rdir, "generate", e.Mod("weather"))
	e.ReadFile(rdir, filepath.Join("gen", weatherPath+".txt"))
}
