# DevContainer Manager

Effectively the goal of the container manager is to:

(a) Bring up plain dev containers in response to tracking requests
(b) Bring up issue containers when an issue is open in a tracked repo
(c) Bring up a PR container when a PR is open within a tracked repo

## Tracking

We track any repo in which brotherlogicautomation is added as a collaborator.
Removal of collaborator access implies turndown of supported containers.

## Separation of concerns

message Container {
	string repo = 1;
	string suffix = 2;
	string prompt = 3;
}

service DevcontainerManagerService {
  rpc Up(UpRequest) return UpResponse;
  rpc Down(DownRequest) return DownResponse;
}

The API just supports bringing up and tearing down containers, the manager
around the API handles the legwork between github and the containers.

