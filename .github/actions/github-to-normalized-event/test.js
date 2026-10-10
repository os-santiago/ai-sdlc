// Test for GitHub to Normalized Event action
const { convert } = require('./index.js');

// Mock GitHub issue webhook payload
const issuePayload = {
  "action": "opened",
  "issue": {
    "id": 123456,
    "number": 42,
    "title": "feat: add new feature",
    "body": "This is a detailed description of the new feature.\n\nIt has multiple lines and should be sufficient for the body requirement.",
    "state": "open",
    "labels": [
      { "name": "ready-to-implement" },
      { "name": "priority-high" }
    ],
    "created_at": "2023-01-01T10:00:00Z",
    "updated_at": "2023-01-01T10:00:00Z"
  },
  "sender": {
    "id": 789012,
    "login": "octocat",
    "type": "User",
    "name": "The Octocat",
    "avatar_url": "https://github.com/images/error/octocat_happy.gif"
  },
  "repository": {
    "id": 345678,
    "full_name": "os-santiago/ai-sdlc",
    "html_url": "https://github.com/os-santiago/ai-sdlc"
  }
};

// Mock GitHub pull_request webhook payload
const prPayload = {
  "action": "opened",
  "pull_request": {
    "id": 987654,
    "number": 123,
    "title": "fix: fix critical bug",
    "body": "This PR fixes a critical bug in the authentication system.\n\nThe bug was causing intermittent failures under load.",
    "state": "open",
    "merged": false,
    "labels": [
      { "name": "bug" },
      { "name": "priority-high" }
    ],
    "created_at": "2023-01-02T14:30:00Z",
    "updated_at": "2023-01-02T14:30:00Z"
  },
  "sender": {
    "id": 789012,
    "login": "octocat",
    "type": "User",
    "name": "The Octocat",
    "avatar_url": "https://github.com/images/error/octocat_happy.gif"
  },
  "repository": {
    "id": 345678,
    "full_name": "os-santiago/ai-sdlc",
    "html_url": "https://github.com/os-santiago/ai-sdlc"
  }
};

console.log('Testing issue event conversion:');
const issueResult = convert(JSON.stringify(issuePayload), 'issues');
console.log(JSON.stringify(issueResult, null, 2));

console.log('\nTesting pull_request event conversion:');
const prResult = convert(JSON.stringify(prPayload), 'pull_request');
console.log(JSON.stringify(prResult, null, 2));