# Bug Matching

We want to move to an issue centric workflow for the container manager. Effectively
we would like to route into both a github project *and* any open issues on that project.

## Tracking

The tracker tracks both (a) the project and (b) any open issues on that project - we
have a placerholder container for (a) (so a container just running under the container name) and for (b) we spin up a container under the name "<project>-<issue_number>". In both cases we initialise the container, run agy and give it an initial prompt

Once the issue is closed we also remove the container. Initially we can track issue state by polling every 10 minutes offset per running container

### Prompts

For the generic container:

"Take a look at the state of this codebase and suggest one improvement or key feature that could be added. Avoid things already mentioned in issues in the github project"

For the issue related container:

"Build state on the codebase, and then look at issue XXX in github and make a suggestion of a potential path forward"

## Open Questions

1. Do we have memory concerns on the host machine? We would expect an average of 5 open issues per container project, can we actually support that many containers running on a given host?

1. How do we pass in the prompt from the container manager into the running container? We can consider adjusting the container configuration to support this if we need to pass something in?