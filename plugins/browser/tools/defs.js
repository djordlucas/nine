export const TOOL_DEFS = [
  {
    name: 'browser_navigate',
    display_name: 'Navigate',
    description: 'Navigate the browser to a URL and wait for the page to load. Use this for ALL web access: to search the web navigate to https://duckduckgo.com/?q=your+query then call browser_extract to read the results. To read any page navigate to its URL then call browser_extract. Returns the final URL, page title, and HTTP status.',
    input_schema: {
      type: 'object',
      required: ['url'],
      properties: {
        url: { type: 'string' },
        wait_until: {
          type: 'string',
          enum: ['load', 'domcontentloaded', 'networkidle'],
          description: 'When to consider navigation complete (default: load)',
        },
        timeout: { type: 'integer', description: 'Timeout in ms (default: BROWSER_TIMEOUT)' },
      },
    },
  },
  {
    name: 'browser_screenshot',
    display_name: 'Screenshot',
    description: 'Take a screenshot of the current browser page. Returns base64-encoded image data.',
    input_schema: {
      type: 'object',
      properties: {
        full_page: { type: 'boolean', description: 'Capture the full scrollable page (default: false)' },
        selector: { type: 'string', description: 'Screenshot only this CSS selector element' },
        quality: { type: 'integer', description: 'JPEG quality 1-100. If set, returns JPEG instead of PNG.' },
      },
    },
  },
  {
    name: 'browser_extract',
    display_name: 'Extract Text',
    description: 'Extract text or structured data from the current page. Returns all visible text by default, or matched element content with a selector.',
    input_schema: {
      type: 'object',
      properties: {
        selector: { type: 'string', description: 'CSS selector. Omit to extract all visible text.' },
        attribute: { type: 'string', description: "Extract this HTML attribute instead of text (e.g. 'href', 'src')" },
        as_json: { type: 'boolean', description: 'Return results as JSON array (default: false)' },
        max_chars: { type: 'integer', description: 'Truncate output to this many characters (default: 50000)' },
      },
    },
  },
  {
    name: 'browser_click',
    display_name: 'Click',
    description: 'Click an element on the current page.',
    input_schema: {
      type: 'object',
      required: ['selector'],
      properties: {
        selector: { type: 'string', description: "CSS or text selector (e.g. 'text=Submit')" },
        timeout: { type: 'integer' },
        wait_for_navigation: { type: 'boolean', description: 'Wait for navigation after click (default: false)' },
      },
    },
  },
  {
    name: 'browser_fill',
    display_name: 'Fill Form',
    description: 'Fill a form field with a value.',
    input_schema: {
      type: 'object',
      required: ['selector', 'value'],
      properties: {
        selector: { type: 'string' },
        value: { type: 'string' },
        clear_first: { type: 'boolean', description: 'Clear existing value before filling (default: true)' },
        timeout: { type: 'integer' },
      },
    },
  },
  {
    name: 'browser_eval',
    display_name: 'Eval JS',
    description: 'Execute JavaScript in the browser page context and return the result as JSON.',
    input_schema: {
      type: 'object',
      required: ['script'],
      properties: {
        script: { type: 'string', description: 'JavaScript expression or function body to evaluate' },
        timeout: { type: 'integer' },
      },
    },
  },
  {
    name: 'browser_wait',
    display_name: 'Wait',
    description: 'Wait for a condition before proceeding: a selector becoming visible, text appearing, or a URL pattern matching. Exactly one of selector, text, or url_pattern must be provided.',
    input_schema: {
      type: 'object',
      properties: {
        selector: { type: 'string', description: 'Wait for this CSS selector to be visible' },
        text: { type: 'string', description: 'Wait for this text to appear on the page' },
        url_pattern: { type: 'string', description: 'Wait for the URL to match this glob (e.g. **/success**)' },
        timeout: { type: 'integer' },
      },
      minProperties: 1,
    },
  },
  {
    name: 'browser_status',
    display_name: 'Browser Status',
    description: 'Return the current browser state: whether a page is open, its URL, and its title.',
    input_schema: { type: 'object', properties: {} },
  },
  {
    name: 'browser_reset',
    display_name: 'Reset Browser',
    description: 'Close the current page and open a fresh one, clearing cookies, localStorage, and session state.',
    input_schema: { type: 'object', properties: {} },
  },
];
