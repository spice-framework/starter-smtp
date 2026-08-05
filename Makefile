.PHONY: check compatibility fmt integration verify

check:
	go run ./internal/qualitygate -mode=check

compatibility:
	go run ./internal/qualitygate -mode=compatibility

fmt:
	go run ./internal/qualitygate -mode=fmt

integration:
	go test -tags=integration -count=1 ./integration/...

verify:
	go run ./internal/qualitygate -mode=verify
