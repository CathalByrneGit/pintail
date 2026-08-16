package version

// version is the release this binary was built from. It is deliberately a var
// rather than a const so a build can stamp it without editing source:
//
//	go build -ldflags "-X main.version=1.2.0" -o pintail .
//
// Every screen header and the CLI read it from here; the string used to be
// duplicated across seven view functions, which is the kind of thing that
// goes stale one header at a time.
var version = "0.1.0"

// versionLabel is the display form ("v0.1.0") used in screen headers.
func Label() string { return "v" + version }
