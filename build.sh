#!/bin/bash -e

[ -z "$USER" ] && echo "Error: env variable USER must be set" && exit 1;
[ -z "$GOPATH" ] && echo "Error: env variable GOPATH must be set" && exit 1;
[ -z "$GITHUB_TOKEN" ] && echo "Warning : env variable GITHUB_TOKEN should be set in the event that a release is to be generated" ;
[ -z ${azure_registry_name+x} ] && echo "Warning : env variable azure_registry_name not set";

if [[ ":$PATH:" != *":$GOPATH/bin:"* ]]; then
    export PATH=$PATH:$GOPATH/bin
fi

if [ -z ${TRAVIS+x} ]; then
    function fold_start() { 
        : 
    }
    function fold_end() { 
        : 
    }
else
    function fold_start() {
        echo -e "travis_fold:start:$1\033[33;1m$2\033[0m"
    }

    function fold_end() {
        echo -e "\ntravis_fold:end:$1\r"
    }
fi

go get -u github.com/golang/dep/cmd/dep

dep ensure

fold_start "build image"
stencil -input Dockerfile | docker build -t runner-build --build-arg USER=$USER --build-arg USER_ID=`id -u $USER` --build-arg USER_GROUP_ID=`id -g $USER` -
fold_end "build image"

# Running build.go inside of a container will result is a simple compilation and no docker images
fold_start build
docker run -e GITHUB_TOKEN=$GITHUB_TOKEN -v $GOPATH:/project runner-build
if [ $? -ne 0 ]; then
    echo ""
    exit $?
fi
fold_end build

# Automatically produces images, and github releases without compilation when run outside of a container
fold_start image
export LOGXI="*=DBG"
go run -tags=NO_CUDA ./build.go -image-only -r cmd
fold_end image

fold_start "image push"
export SEMVER=`semver`
if docker image inspect sentient-technologies/studio-go-runner/runner:$SEMVER 2>/dev/null 1>/dev/null; then
    if type aws 2>/dev/null ; then
        `aws ecr get-login --no-include-email --region us-west-2`
        if [ $? -eq 0 ]; then
            account=`aws sts get-caller-identity --output text --query Account`
            if [ $? -eq 0 ]; then
                docker tag sentient-technologies/studio-go-runner/runner:$SEMVER $account.dkr.ecr.us-west-2.amazonaws.com/sentient-technologies/studio-go-runner/runner:$SEMVER
                docker push $account.dkr.ecr.us-west-2.amazonaws.com/sentient-technologies/studio-go-runner/runner:$SEMVER
            fi
        fi
    fi
    if type az 2>/dev/null; then
        if [ -z ${azure_registry_name+x} ]; then  
            :
        else
            if az acr login --name $azure_registry_name; then
                docker tag sentient-technologies/studio-go-runner/runner:$SEMVER $azure_registry_name.azurecr.io/sentient.ai/studio-go-runner/runner:$SEMVER
                docker push $azure_registry_name.azurecr.io/sentient.ai/studio-go-runner/runner:$SEMVER
            fi
        fi
    fi
fi
fold_end "image push"
