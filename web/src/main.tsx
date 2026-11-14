import React from 'react'
import ReactDOM from 'react-dom/client'
import './mdui'
import 'virtual:uno.css'
import '@fastrd/ui-foundation/mdui.css'
import { App } from './App'
import './styles.css'
import './workbench-theme.css'

ReactDOM.createRoot(document.getElementById('root')!).render(<React.StrictMode><App /></React.StrictMode>)
