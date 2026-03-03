.PHONY: build test run clean deploy

build:
	CGO_ENABLED=0 go build -o vault-mirror-service ./main.go

test:
	go test ./... -v -count=1

run:
	go run ./main.go

clean:
	rm -f vault-mirror-service
	rm -f mirror-service-task-definition-updated.json

deploy:
	bash deploy.sh
