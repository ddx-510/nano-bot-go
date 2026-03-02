# Heartbeat Tasks

Check the following and report any issues:

- [ ] Check service health: `query_api("my-service", "/health")`
- [ ] Look for recent errors in repos: `exec("cd repos/my-repo && git log --oneline -5")`
- [ ] If any service is down or errors found, send a summary to the team
- [ ] If everything is healthy, do nothing (don't spam the channel)
