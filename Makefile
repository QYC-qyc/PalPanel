.PHONY: test build cross

test:
	go test ./... -race

build:
	go build -trimpath -o panel .

cross:
	bash scripts/build.sh
