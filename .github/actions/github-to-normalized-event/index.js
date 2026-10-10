// We'll use only Node.js built-in modules.
const crypto = require('crypto');

/**
 * Convert GitHub webhook payload to AI-SDLC normalized event
 * @param {string} webhookPayloadStr - JSON string of the GitHub webhook payload
 * @param {string} eventName - GitHub event name (e.g., 'issues', 'pull_request')
 * @returns {Object} Normalized event object
 */
function convert(webhookPayloadStr, eventName) {
  const payload = JSON.parse(webhookPayloadStr);
  const normalized = {
    version: '1.0.0',
    entity: {},
    transition: '',
    actor: {},
    repository: null
  };

  // Common: actor information
  const actorPayload = payload.sender || {};
  normalized.actor = {
    id: String(actorPayload.id || ''),
    type: actorPayload.type === 'Bot' ? 'bot' : 'user',
    login: actorPayload.login || null,
    display_name: actorPayload.name || null,
    avatar_url: actorPayload.avatar_url || null
  };

  // Common: repository information
  if (payload.repository) {
    normalized.repository = {
      id: String(payload.repository.id || ''),
      name: payload.repository.full_name || '',
      url: payload.repository.html_url || null
    };
  }

  // Event-specific mapping
  switch (eventName) {
    case 'issues':
      return convertIssue(payload, normalized);
    case 'pull_request':
      return convertPullRequest(payload, normalized);
    default:
      throw new Error(`Unsupported event name: ${eventName}`);
  }
}

/**
 * Convert GitHub issue event to normalized event (work_item)
 * @param {Object} payload - GitHub issue webhook payload
 * @param {Object} normalized - Base normalized event object to fill
 * @returns {Object} Filled normalized event object
 */
function convertIssue(payload, normalized) {
  const issue = payload.issue || {};
  normalized.entity = {
    kind: 'work_item',
    id: String(issue.id || ''),
    number: issue.number || null,
    title: issue.title || '',
    description: issue.body || null,
    state: issue.state === 'open' ? 'open' : 'closed', // GitHub: open/closed -> neutral: open/closed
    labels: issue.labels ? issue.labels.map(l => l.name) : null,
    created_at: issue.created_at || null,
    updated_at: issue.updated_at || null
  };

  // Map GitHub action to neutral transition
  // See: https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads#issue_event
  const actionMap = {
    opened: 'opened',
    edited: 'updated', // We don't have an 'updated' in our list, but we can use it or map to something else? Let's use 'updated' for now.
    labeled: 'labeled',
    unlabeled: 'unlabeled',
    assigned: 'assigned',
    unassigned: 'unassigned',
    reopened: 'reopened',
    closed: 'closed'
  };
  normalized.transition = actionMap[payload.action] || payload.action;

  return normalized;
}

/**
 * Convert GitHub pull_request event to normalized event (change_request)
 * @param {Object} payload - GitHub pull_request webhook payload
 * @param {Object} normalized - Base normalized event object to fill
 * @returns {Object} Filled normalized event object
 */
function convertPullRequest(payload, normalized) {
  const pr = payload.pull_request || {};
  normalized.entity = {
    kind: 'change_request',
    id: String(pr.id || ''),
    number: pr.number || null,
    title: pr.title || '',
    description: pr.body || null,
    state: pr.merged ? 'merged' : pr.state === 'open' ? 'open' : 'closed', // GitHub: open/closed, but we add merged
    labels: pr.labels ? pr.labels.map(l => l.name) : null,
    created_at: pr.created_at || null,
    updated_at: pr.updated_at || null
  };

  // Map GitHub action to neutral transition
  // See: https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads#pull_request_event
  const actionMap = {
    opened: 'opened',
    edited: 'updated',
    synchronized: 'synchronized',
    labeled: 'labeled',
    unlabeled: 'unlabeled',
    assigned: 'assigned',
    unassigned: 'unassigned',
    reopened: 'reopened',
    closed: 'closed'
  };
  normalized.transition = actionMap[payload.action] || payload.action;

  return normalized;
}

// Export for use in other modules
module.exports = {
  convert
};

// Main execution when run directly (e.g., via GitHub Action)
if (require.main === module) {
  try {
    const webhookPayload = process.env.webhook_payload;
    const eventName = process.env.event_name;

    if (!webhookPayload || !eventName) {
      throw new Error('Missing required inputs: webhook_payload and event_name');
    }

    const result = convert(webhookPayload, eventName);
    // Output as JSON string
    process.stdout.write(JSON.stringify(result));
  } catch (error) {
    console.error(`Error converting webhook to normalized event: ${error.message}`);
    process.exit(1);
  }
}