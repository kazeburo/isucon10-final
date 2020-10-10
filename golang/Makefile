PREFIX=$(PWD)
BINDIR=$(PREFIX)/bin

COMPILER=go

GOFILES=$(wildcard ./*.go ./**/*.go)

.PHONY: all
all: clean build ## Cleanup and Build

.PHONY: build
build: $(GOFILES) ## Build executable files
	@GOBIN=$(BINDIR) $(COMPILER) install ./cmd/...  


.PHONY: clean
clean: ## Cleanup files
	@$(RM) -r $(BINDIR)

.PHONY: help
help: ## Show help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.PHONY: restart
restart: build
	ssh -t 10.162.47.102 sudo systemctl stop xsuportal-api-golang.service xsuportal-web-golang.service 
	scp bin/xsuportal 10.162.47.102:webapp/golang/bin/xsuportal
	scp bin/benchmark_server 10.162.47.102:webapp/golang/bin/benchmark_server
	ssh -t 10.162.47.102 sudo systemctl start xsuportal-api-golang.service xsuportal-web-golang.service
	ssh -t 10.162.47.103 sudo systemctl stop xsuportal-api-golang.service xsuportal-web-golang.service 
	scp bin/xsuportal 10.162.47.103:webapp/golang/bin/xsuportal
	scp bin/benchmark_server 10.162.47.103:webapp/golang/bin/benchmark_server
	ssh -t 10.162.47.103 sudo systemctl start xsuportal-api-golang.service xsuportal-web-golang.service
	sudo systemctl stop xsuportal-api-golang.service
	sudo systemctl stop xsuportal-web-golang.service
	
