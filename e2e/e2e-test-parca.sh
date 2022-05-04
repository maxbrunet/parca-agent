################################################################################
#
# This script is meant to be run from the root of this project
#
################################################################################

#!/bin/bash

set -euo pipefail

function e2erun() {
  minikube_cluster_up
  eval $(minikube -p parca-dev docker-env)
  deploy
  # minikube_cluster_down genius, spin deploy and delete ugh
}

# Create local minikube cluster and deploys the dev env for parca and parca agent 
function minikube_cluster_up() {
  echo "Spinning up a parca dev cluster"
  minikube start -p parca-dev \
    --driver=kvm2 \
    --kubernetes-version=v1.22.3 \
    --cpus=4 \
    --memory=16gb \
    --disk-size=20gb \
    --docker-opt dns=8.8.8.8 \
    --docker-opt default-ulimit=memlock=9223372036854775807:9223372036854775807
}

# Delete minikube instance
function minikube_cluster_down() {
  echo "deleting parca dev cluster"
  minikube delete -p parca-dev
}

# Fetches latest binaries for parca and parca agent 
function deploy() {
  SERVER_LATEST_VERSION=$(curl -s https://api.github.com/repos/parca-dev/parca/releases/latest | grep -oP '"tag_name": "\K(.*)(?=")' | xargs echo -n)
  AGENT_LATEST_VERSION=$(curl -s https://api.github.com/repos/parca-dev/parca-agent/releases/latest | grep -oP '"tag_name": "\K(.*)(?=")' | xargs echo -n)

  echo "Server version: $SERVER_LATEST_VERSION"
  echo "Agent version: $AGENT_LATEST_VERSION"

  build

  kubectl apply -f ./manifests/local/manifest-e2e.yaml
  kubectl apply -f https://github.com/parca-dev/parca-agent/releases/download/"$AGENT_LATEST_VERSION"/kubernetes-manifest.yaml

  sleep 1m

  echo "Connecting to Parca"

  kubectl port-forward -n parca service/parca 7070
}

function vendor() {
  jb install
}

function build() {
  #  ifeq ($(GITHUB_BRANCH_NAME),)
  #    BRANCH=$(shell git rev-parse --abbrev-ref HEAD)-
  #  else
  #    BRANCH=$(GITHUB_BRANCH_NAME)-
  #  endif
  #  ifeq ($(GITHUB_SHA),)
  #    COMMIT=$(shell git describe --no-match --dirty --always --abbrev=8)
  #  else
  #    COMMIT=$(shell echo $(GITHUB_SHA) | cut -c1-8)
  #  endif
  #  VERSION=$(if $(RELEASE_TAG),$(RELEASE_TAG),$(shell $(CMD_GIT) describe --tags 2>/dev/null || echo '$(BRANCH)$(COMMIT)'))

  VERSION="sumolocal" #"$1"
  COMMIT="$(git describe --no-match --dirty --always --abbrev=8)"
  OUT_DOCKER="ghcr.io/parca-dev/parca"

  echo $COMMIT

  # for amd64
  DOCKER_GOLANG_ALPINE_SHAS=('docker.io/golang@sha256:7473adb02bd430045c938f61e2c2177ff62b28968579dfed99085a0960f76f5d')
  DOCKER_NODE_ALPINE_SHAS=('docker.io/library/node@sha256:10ef59da5b5ccdbaff99a81df1bcccb0500723633ce406efed6f1fb74adc8568')
  DOCKER_ALPINE_SHAS=('docker.io/alpine@sha256:e7d88de73db3d3fd9b2d63aa7f447a10fd0220b7cbf39803c803f2af9ba256b3')

  docker build  \
        --build-arg NODE_BUILDER_BASE=$DOCKER_NODE_ALPINE_SHAS \
        --build-arg GOLANG_BUILDER_BASE=$DOCKER_GOLANG_ALPINE_SHAS \
        --build-arg RUNNER_BASE=$DOCKER_ALPINE_SHAS \
        --build-arg VERSION=$VERSION  \
        --build-arg COMMIT=$COMMIT \
        -t $OUT_DOCKER:$VERSION ./../ 


  echo blah
  rm -rf manifests/local 
  mkdir -p manifests/local

  echo blahhh
  jsonnet --tla-str version="sumolocal" -J vendor main.jsonnet -m manifests/local | xargs -I{} sh -c 'cat {} | gojsontoyaml > {}.yaml; rm -f {}' -- {}
  awk 'BEGINFILE {print "---"}{print}' manifests/local/* > manifests/local/manifest-e2e.yaml	
}
