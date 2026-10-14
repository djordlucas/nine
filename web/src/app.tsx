import { useEffect } from 'preact/hooks';
import { LocationProvider, Router, Route } from 'preact-iso';
import { auth, checkSession } from './api/client';
import { connect, disconnect } from './api/events';
import { Shell } from './components/Shell';
import { Dashboard } from './pages/Dashboard';
import { Journal } from './pages/Journal';
import { Login } from './pages/Login';
import { NotFound, Pending } from './pages/Pending';

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
          <Route path="/conversations/*" component={Pending} />
          <Route path="/processes/*" component={Pending} />
          <Route path="/goals/*" component={Pending} />
          <Route path="/tools/*" component={Pending} />
          <Route path="/inbox/*" component={Pending} />
          <Route path="/journal" component={Journal} />
          <Route path="/system/*" component={Pending} />
          <Route default component={NotFound} />
        </Router>
      </Shell>
    </LocationProvider>
  );
}
