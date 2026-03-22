---
description: Commit and push changes to a new branch
---
When finishing a task, run the following steps:

1. Build the code to ensure it compiles successfully:
   `go build`

// turbo
2. Create and checkout a new branch with a descriptive name:
   `git checkout -b <descriptive-branch-name>`

// turbo
3. Commit the changes:
   `git commit -am "<descriptive commit message>"`

// turbo
4. Push the changes to the newly created branch:
   `git push -u origin HEAD`
