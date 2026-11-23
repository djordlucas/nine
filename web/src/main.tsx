import { render } from 'preact';
import '@fontsource-variable/inter';
import './tokens.css';
import './theme';
import { App } from './app';

render(<App />, document.getElementById('app')!);
