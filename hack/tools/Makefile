BIN_DIR := bin
GO ?= go

.PHONY: clusteradm
clusteradm: $(BIN_DIR)/clusteradm

$(BIN_DIR):
	mkdir -p $@

$(BIN_DIR)/clusteradm: go.mod go.sum tools.go | $(BIN_DIR)
	GOWORK=off CGO_ENABLED=0 $(GO) build -tags=tools -o $@ open-cluster-management.io/clusteradm/cmd/clusteradm

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)
