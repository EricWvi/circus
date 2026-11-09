.PHONY: gen lint test check-gen build

gen:
	buf generate

lint:
	buf lint
	go vet ./...
	@files="$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))"; \
		if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

test:
	go test ./...

# Fails if generation changed tracked files or produced untracked ones.
check-gen: gen
	@git diff --exit-code --stat -- gen/
	@untracked="$$(git ls-files --others --exclude-standard -- gen/)"; \
		if [ -n "$$untracked" ]; then printf '%s\n' "$$untracked"; exit 1; fi

build:
	go build -o bin/circus ./cmd/circus
