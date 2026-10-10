// Simple test for GitHub to Normalized Event action
const { convert } = require('./index.js');

console.log('Testing simple conversion...');

// Minimal GitHub issue webhook payload
const minimalPayload = {
  "action": "opened",
  "issue": {
    "id": 1,
    "number": 1,
    "title": "test",
    "body": "test body",
    "state": "open",
    "labels": [],
    "created_at": "2023-01-01T00:00:00Z",
    "updated_at": "2023-01-01T00:00:00Z"
  },
  "sender": {
    "id": 1,
    "login": "testuser",
    "type": "User",
    "name": "Test User",
    "avatar_url": ""
  },
  "repository": {
    "id": 1,
    "full_name": "test/test",
    "html_url": "https://github.com/test/test"
  }
};

try {
  const result = convert(JSON.stringify(minimalPayload), 'issues');
  console.log('Success! Result:');
  console.log(JSON.stringify(result, null, 2));
} catch (error) {
  console.error('Error:', error.message);
  console.error(error.stack);
}