import { render } from 'preact';
import { AdminApp } from './App';
import './admin.css';

render(<AdminApp />, document.getElementById('admin-app')!);
