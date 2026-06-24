# Kubernetes

Moving devcontainer-manager into kubernetes means a couple of things:

1. We keep the core logic and the ability for devcontainer-manager to run on a server.
   The kubernetes addition replicates the logic in it's own container that we can
   run within a kubernetes pod also.
1. The kubernetes version is responsible for:
   1. Bringing itself up
   1. Responding to API events and dealing with those events
   1. Running the same processing loop to capture things we missed in the event processing.
1. The API offers three methods:
   1. List - shows the running containers and their configuration (Same up config)
   1. Up - brings up a devcontainer given:
      1. Repo
      1. Identifier
         1. Issue number
	 1. PR number
      1. Branch
      1. Prompt
         1. Prompt Text
	 1. Model
	 1. Harness
   1. Down - brings down a container given:
      1. Container id

The config pieces are:

1. Repo - the underlying github repo which houses the devcontainer
1. Identifier (optional) - an extra slice for the repo (e.g. issue number, PR number etc.)
1. Branch (optional) - if given, when the container starts it will sync to head, and automatically branch out to this value
1. Prompt (optional) - if given, once the container is started and branched, we will run this interactive prompt using the supplied Model (defaults to default model) and harness (defaults to antigravity).

When receiving an API event, the manager stores the configuration internally, marks the container as being received, adds a "dcm-received" label to the issue or PR. It then waits for the thread pool to have a spare thread. Once it's aquired a thread it sends the "dcm-creating" label, and brings the container up. If the container fails it sends "dcm-failed" label and files a bug in the repo detailing why the container failed and associated logs. If the container successfully starts then we (a) attempt to branch (sending label dcm-branching / dcm-branch-failed / dcm-branch-succeeded) using the same logic in the existing devcontainer-manager, and then initiate the harness (dcm-harness-starting / dcm-harness-failed / dcm-harness-succeeded).

Once we've reached the end of the process and if everything has completed we remove *all* the dcm- labels and replace it with just dcm-ready. We retry the container init process if we get "dcm-failed" but do not retry if the branching or harness fails.

When bringing a container down, we just use the container id we set on creation - we rely on the API client to know which containers are running (they can get this from the list function). List returns the container IDs alongside the configuration used to create them.

Tech Stack:

1. brotherlogic/pstore for persistent storage of container configs
1. devpod-cli for managing containers (using the kubernetes platform the manager is running on)
1. gh with a master key for label handling - we have a key which gives us labeling
1. access to a gh key to inject into the container
1. Credentials in the manager to be injected into the container as necessary
1. Uses grpc/protobug as underlying storage / comms