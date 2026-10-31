test:
    go test -race -p 4 -timeout 2m ./...

integration:
    go test -count=1 -timeout 30m -run TestProvisioning -v ./cmd/fleeting-plugin-libvirt

build:
    go build -o bin/ ./...

lint:
    golangci-lint run ./...
    go vet ./...

brand:
    bash ~/gitlab.com/dunn.dev/reference/repository/scripts/brand.sh
