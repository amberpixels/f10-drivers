# standardgo carries the ruleset and the golangci-lint engine in one binary, so this
# repo holds no lint config of its own.
standardgo := "github.com/amberpixels/standardgo/cmd/standardgo@v0.2.3"

# format Go code
fmt:
    go run {{ standardgo }} fmt ./...

# lint Go code - reports findings, changes nothing
lint:
    go run {{ standardgo }} ./...

# auto-fix what can be fixed - run on a clean tree and read the diff
fix:
    go run {{ standardgo }} ./... --fix

# run tests
test:
    go test ./...

# build the binary into bin/
build:
    go build -o bin/f10-drivers ./cmd/f10-drivers

# install f10-drivers into GOPATH/bin
install:
    go install ./cmd/f10-drivers

# run all checks - read-only
ci: lint test
