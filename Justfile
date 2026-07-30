default: run

init:
    go mod tidy

build:
    go build -o ship ./cmd/ship

run *args: 
    go run ./cmd/ship {{args}}

check:
    go vet ./...
    go build ./cmd/ship

clean:
    rm -f ship

# watch mode — rerun on save
watch:
    which entr >/dev/null 2>&1 || (echo "install entr: apt/brew install entr" && exit 1)
    ls cmd/ship/*.go internal/**/*.go | entr -r just run
