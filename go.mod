module github.com/LackOfMorals/graphlite

go 1.24.0

require (
	// antlr cannot be upgraded: cloudprivacylabs/opencypher@v1.0.0 generated parser
	// requires DeserializeFromUInt16 which was removed in antlr4-go v1.4.10.
	github.com/antlr/antlr4/runtime/Go/antlr v0.0.0-20210803070921-b358b509191a
	github.com/cloudprivacylabs/opencypher v1.0.0
	github.com/cucumber/godog v0.15.1
	modernc.org/sqlite v1.35.0
)

require (
	github.com/cucumber/gherkin/go/v26 v26.2.0 // indirect
	github.com/cucumber/messages/go/v21 v21.0.1 // indirect
	github.com/gofrs/uuid v4.3.1+incompatible // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-memdb v1.3.4 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/spf13/pflag v1.0.7 // indirect
)

require (
	github.com/cloudprivacylabs/lpg/v2 v2.0.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/nleeper/goment v1.4.4 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tkuchiki/go-timezone v0.2.0 // indirect
	golang.org/x/exp v0.0.0-20230315142452-642cacee5cc0 // indirect
	// golang.org/x/sys: GO-2026-5024 fixed in v0.44.0 which requires Go 1.25;
	// staying on v0.41.0 until minimum Go version is raised.
	golang.org/x/sys v0.41.0 // indirect
	modernc.org/libc v1.61.13 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.8.2 // indirect
	pgregory.net/rapid v1.3.0
)
