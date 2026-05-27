.DEFAULT_GOAL := help

# Use sudo for docker calls if the current user isn't in the docker group.
DOCKER := $(shell if docker info >/dev/null 2>&1; then echo docker; else echo sudo docker; fi)
COMPOSE := $(DOCKER) compose

help:  ## show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_.-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

install:  ## install dependencies, configure .env, build + start (interactive)
	bash scripts/install.sh

install-yes:  ## same as install, no prompts (uses host IP for RELAY_PUBLIC_URL)
	bash scripts/install.sh --yes

api-check:  ## verify the upstream Nextcloud App Store API still matches what we expect
	bash scripts/api-check.sh

start:  ## start the relay (build if needed)
	$(COMPOSE) up -d --build

stop:  ## stop the relay
	$(COMPOSE) down

restart:  ## restart the relay (e.g. after .env change)
	$(COMPOSE) up -d --build --force-recreate

logs:  ## follow container logs
	$(COMPOSE) logs -f --tail=200

status:  ## show container status + cache size
	@$(COMPOSE) ps
	@echo
	@echo "Cache volume usage:"
	@$(DOCKER) run --rm -v nextcloud-appstore-relay_relay-cache:/c alpine du -sh /c 2>/dev/null || true

update:  ## pull repo changes, rebuild image, restart
	git pull --ff-only
	$(COMPOSE) build --pull
	$(COMPOSE) up -d --force-recreate

clean-cache:  ## delete the on-disk cache (relay will re-fetch everything on next request)
	$(COMPOSE) down
	$(DOCKER) volume rm nextcloud-appstore-relay_relay-cache || true
	$(COMPOSE) up -d

.PHONY: help install install-yes api-check start stop restart logs status update clean-cache
