.PHONY: build run clean tidy

build:
	go build -o ai-server .

run: build
	./ai-server

tidy:
	go mod tidy

clean:
	rm -f ai-server
	rm -f data/ai-server.db
