#!/bin/bash -e

[ -z "$USER" ] && echo "Error: env variable USER must be set" && exit 1;
[ -z "$GOPATH" ] && echo "Error: env variable GOPATH must be set" && exit 1;
[ -z "$GITHUB_TOKEN" ] && echo "Warning : env variable GITHUB_TOKEN should be set in the event that a release is to be generated" ;
[ -z ${azure_registry_name+x} ] && echo "Warning : env variable azure_registry_name not set";

if [[ ":$PATH:" != *":$GOPATH/bin:"* ]]; then
    export PATH=$PATH:$GOPATH/bin
fi

export LOGXI="*=DBG"
export LOGXI_FORMAT="happy,maxcol=1024"

[ -z "$TERM" ] && export TERM=xterm+256color;

if [ -n "$(type -t travis_fold)" ] && [ "$(type -t travis_fold)" = function ]; then
:
else
    function travis_fold() {
        : 
    }
    function travis_nanoseconds() {
        : 
    }
    function travis_time_start() {
        : 
    }
    function travis_time_finish() {
        : 

    }
fi

go get -u github.com/golang/dep/cmd/dep

dep ensure

travis_fold start "build.image"
    travis_time_start
        stencil -input Dockerfile | docker build -t runner-build --build-arg USER=$USER --build-arg USER_ID=`id -u $USER` --build-arg USER_GROUP_ID=`id -g $USER` -
    travis_time_finish
travis_fold end "build.image"

# Running build.go inside of a container will result is a simple compilation and no docker images
travis_fold start "build"
    travis_time_start
        docker run -e TERM="$TERM" -e LOGXI="$LOGXI" -e LOGXI_FORMAT="$LOGXI_FORMAT" -e GITHUB_TOKEN=$GITHUB_TOKEN -v $GOPATH:/project runner-build
        if [ $? -ne 0 ]; then
            echo ""
            exit $?
        fi
    travis_time_finish
travis_fold end "build"

# Automatically produces images, and github releases without compilation when run outside of a container
travis_fold start "image.build"
    travis_time_start
        go run -tags=NO_CUDA ./build.go -image-only -r cmd
    travis_time_finish
travis_fold end "image.build"

travis_fold start "image.push"
    travis_time_start
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
    travis_time_finish
travis_fold end "image.push"