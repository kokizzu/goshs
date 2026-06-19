.PHONY: generate security fmt fmt-check vet check new-version run-unit run-unit-no-network run-integration clean-tests run-tests run install clean

# esbuild and sass needed
generate:
	@echo "[*] Bundling JS with esbuild"
	@esbuild assets/js/src/main.js --bundle --minify --outfile=httpserver/static/js/main.min.js
	@echo "[*] Compiling SCSS"
	@sass --no-source-map -s compressed assets/css/src/main.scss httpserver/static/css/style.css
	@echo "[OK] Done bundling and compiling things"
	@echo "[*] Copying embedded files to target location"
	@rm -rf httpserver/embedded
	@cp -r embedded httpserver/

security:
	@echo "[*] Checking with gosec"
	@gosec ./...
	@echo "[OK] No issues detected"

fmt:
	@gofmt -w .
	@echo "[OK] Formatted"

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "[!] Needs gofmt:"; gofmt -l .; exit 1; }
	@echo "[OK] Formatting clean"

vet:
	@echo "[*] Running go vet"
	@go vet ./...
	@echo "[OK] go vet clean"

# on-demand quality gate (formatting + vet); security and tests are run separately
check: fmt-check vet

new-version:
ifndef VERSION
	$(error Usage: make new-version VERSION=vX.Y.Z)
endif
	@echo "Updating version to $(VERSION)..."
	@sed -i 's/var GoshsVersion = "v[^"]*"/var GoshsVersion = "$(VERSION)"/' goshsversion/version.go
	@SPECVER=$$(echo "$(VERSION)" | sed 's/^v//'); \
	DATE=$$(date '+%a %b %d %Y'); \
	sed -i "s|^Version:.*|Version:        $$SPECVER|" packaging/rpm/goshs.spec; \
	sed -i "/^%changelog/a * $$DATE Patrick Hener <patrickhener@gmx.de> - $$SPECVER-1\n- Add new version $(VERSION)" packaging/rpm/goshs.spec
	@git add goshsversion/version.go packaging/rpm/goshs.spec
	@git commit -m "New version $(VERSION)"
	@git push
	@git tag -a $(VERSION) -m "Release $(VERSION)"
	@git push origin $(VERSION)
	@echo "[*] Tag pushed. Release binaries, packages, and Docker images are built and published by CI."
	@echo "    See .github/workflows/release.yml and docker-release.yml"

run-unit: clean-tests
	@go test $$(go list ./... | grep -v /integration) -count=1

run-unit-no-network:
	@go test -short $$(go list ./... | grep -v /integration) -count=1

run-integration: clean-tests
	@go test -v ./integration -count=1

clean-tests:
	@mkdir -p ./integration/files
	@rm -rf ./integration/files/*
	@cp ./integration/keepFiles/test_data.txt ./integration/files/
	@rm -rf ./sftpserver/testdir
	@rm -f ./sftpserver/test.txt
	@echo "cleaned up, ready for next test"

run-tests: run-unit run-integration

run:
	@go run main.go

install:
	@go install ./...
	@echo "[OK] Application was installed to go binary directory!"

clean:
	@rm -rf ./dist
	@echo "[OK] Cleaned up!"
