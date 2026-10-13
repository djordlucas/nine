import { render } from 'preact';
import './tokens.css';
import './theme';
import { App } from './app';

render(<App />, document.getElementById('app')!);
