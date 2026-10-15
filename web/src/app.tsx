import { useEffect } from 'preact/hooks';
import { LocationProvider, Router, Route } from 'preact-iso';
import { auth, checkSession } from './api/client';
import { connect, disconnect } from './api/events';
import { Shell } from './components/Shell';
import { Dashboard } from './pages/Dashboard';
import { Conversations } from './pages/Conversations';
import { Goals } from './pages/Goals';
import { Inbox } from './pages/Inbox';
import { Journal } from './pages/Journal';
import { Login } from './pages/Login';
import { NotFound, Pending } from './pages/Pending';
import { Processes } from './pages/Processes';
import { Tools } from './pages/Tools';
import { Turn } from './pages/Turn';

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
          <Route path="/" component={Dashboard} />
          <Route path="/conversations/:id/turns/:n" component={Turn} />
          <Route path="/conversations/:id" component={Conversations} />
          <Route path="/conversations" component={Conversations} />
          <Route path="/processes/:id" component={Processes} />
          <Route path="/processes" component={Processes} />
          <Route path="/goals/workflows/:id" component={Goals} />
          <Route path="/goals/:id" component={Goals} />
          <Route path="/goals" component={Goals} />
          <Route path="/tools/plugins" component={Tools} />
          <Route path="/tools/skills/:name" component={Tools} />
          <Route path="/tools/skills" component={Tools} />
          <Route path="/tools/:name" component={Tools} />
          <Route path="/tools" component={Tools} />
          <Route path="/inbox" component={Inbox} />
          <Route path="/journal" component={Journal} />
          <Route path="/system/*" component={Pending} />
          <Route default component={NotFound} />
        </Router>
      </Shell>
    </LocationProvider>
  );
}
