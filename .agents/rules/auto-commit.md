---
description: Always commit, push changes, and request review at the end of a task
---
When completing any task, ALWAYS follow these steps:

1. **Commit and Push:**
   - If you are currently on the 'main' branch, create a new branch with an appropriate name first before committing and pushing.
   - If you are on a different branch, commit and push directly to that branch.
2. **Find the Pull Request:** Once you have pushed a change, use the `gh` tool to check for the pull request for your branch.
3. **Request Review:** Once you find the pull request, add a comment to the PR that says `/gemini-review`. You can use the `gh pr comment` command for this.
