NAME=goosh
VERSION=0.2.2

prepare: # Download deps
	@go get github.com/golang/dep/cmd/dep
	@dep ensure

build: # Build goosh binary
	@CGO_ENABLED=0 GOOS=linux go build -o goosh-linux-amd64 cmd/server/server.go

clean: ## Tidy up
	@rm -f goosh-linux-amd64
	@rm -rf tmp

publish: clean build tag
	docker build \
		--tag michelefinotto/goosh:$(VERSION) \
		--tag michelefinotto/goosh:latest .
	docker push michelefinotto/goosh:$(VERSION)
	docker push michelefinotto/goosh:latest

tag:
	git tag v$(VERSION) && git push --tags
