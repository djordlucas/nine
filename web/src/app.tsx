import { useEffect } from 'preact/hooks';
import { LocationProvider, Router, Route, useLocation, useRoute } from 'preact-iso';
import { api, auth, checkSession, get } from './api/client';
import { connect, disconnect } from './api/events';
import { ConfirmHost } from './components/Confirm';
import { Shell } from './components/Shell';
import { Builder } from './pages/Builder';
import { Data } from './pages/Data';
import { Agents } from './pages/Agents';
import { Inbox } from './pages/Inbox';
import { Journal } from './pages/Journal';
import { Login } from './pages/Login';
import { NotFound } from './pages/NotFound';
import { System } from './pages/System';
import { Tools } from './pages/Tools';
import { param } from './pages/Processes';

/** "/" lands on the Agents page while the Dashboard is away. */
function Home() {
  const { route } = useLocation();
  useEffect(() => {
    route('/conversations', true);
  }, []);
  return null;
}

/** Old /goals links: a goal is its process, so land on the Agents page. */
function GoalRedirect() {
  const { params } = useRoute();
  const { route } = useLocation();
  const id = param(params.id as string | undefined) ?? '';
  useEffect(() => {
    route(id ? `/processes/${encodeURIComponent(`goal:${id}`)}` : '/conversations', true);
  }, [id]);
  return null;
}

/** Old /goals/workflows/:id links: workflows show in the conversation that runs them. */
function WorkflowRedirect() {
  const { route } = useLocation();
  useEffect(() => {
    route('/conversations', true);
  }, []);
  return null;
}

/** Old /board links: a board is local to its session, so land on that agent's page. */
function BoardRedirect() {
  const { params } = useRoute();
  const { route } = useLocation();
  const id = param(params.id as string | undefined);
  useEffect(() => {
    if (!id) {
      route('/conversations', true);
      return;
    }
    void get(api.GET('/board/{id}', { params: { path: { id } } }), 'the task')
      .then((t) => route(t?.posted_by ? `/conversations/${encodeURIComponent(t.posted_by)}` : '/conversations', true))
      .catch(() => route('/conversations', true));
  }, [id]);
  return null;
}

export function App() {
  useEffect(() => {
    void checkSession();
  }, []);

  // One event stream per tab, open while signed in.
  const signedIn = auth.value === 'signed-in';
  useEffect(() => {
    if (!signedIn) return;
    connect();
    return disconnect;
  }, [signedIn]);

  switch (auth.value) {
    case 'checking':
      return null;
    case 'unreachable':
      return (
        <div class="card standalone" role="alert">
          <p>The Nine API is not answering. Is the daemon running?</p>
          <button class="button" type="button" onClick={() => void checkSession()}>
            Retry
          </button>
        </div>
      );
    case 'signed-out':
      return <Login />;
  }

  return (
    <LocationProvider>
      <Shell>
        <Router>
          <Route path="/" component={Home} />
          <Route path="/conversations/:id" component={Agents} />
          <Route path="/conversations" component={Agents} />
          <Route path="/processes/:id/session" component={Agents} />
          <Route path="/processes/:id" component={Agents} />
          <Route path="/processes" component={Agents} />
          <Route path="/board/new" component={BoardRedirect} />
          <Route path="/board/:id" component={BoardRedirect} />
          <Route path="/board" component={BoardRedirect} />
          <Route path="/goals/workflows/:id" component={WorkflowRedirect} />
          <Route path="/goals/:id" component={GoalRedirect} />
          <Route path="/goals" component={GoalRedirect} />
          <Route path="/tools/plugins" component={Tools} />
          <Route path="/tools/skills/:name" component={Tools} />
          <Route path="/tools/skills" component={Tools} />
          <Route path="/tools/:name" component={Tools} />
          <Route path="/tools" component={Tools} />
          <Route path="/inbox" component={Inbox} />
          <Route path="/data/database/:table" component={Data} />
          <Route path="/data/*" component={Data} />
          <Route path="/data" component={Data} />
          <Route path="/journal" component={Journal} />
          <Route path="/builder/agents/:id" component={Builder} />
          <Route path="/builder/agents" component={Builder} />
          <Route path="/builder/roles/:name" component={Builder} />
          <Route path="/builder/roles" component={Builder} />
          <Route path="/builder/skills/:name" component={Builder} />
          <Route path="/builder/skills" component={Builder} />
          <Route path="/builder/data" component={Builder} />
          <Route path="/builder" component={Builder} />
          <Route path="/system/sessions/:id" component={System} />
          <Route path="/system/docs/:id" component={System} />
          <Route path="/system/spec/:id" component={System} />
          <Route path="/system/*" component={System} />
          <Route path="/system" component={System} />
          <Route default component={NotFound} />
        </Router>
        <ConfirmHost />
      </Shell>
    </LocationProvider>
  );
}
