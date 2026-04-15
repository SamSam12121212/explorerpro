import ReactDOM from "react-dom/client";
import { BrowserRouter, Route, Routes } from "react-router";
import App from "./App";
import { chatStore } from "./chatStore";
import "./styles.css";

const rootElement = document.getElementById("root");

if (!rootElement) {
  throw new Error('Missing root element with id "root"');
}

chatStore.initialize();

ReactDOM.createRoot(rootElement).render(
  <BrowserRouter>
    <Routes>
      <Route path="/" element={<App />} />
      <Route path="/collections" element={<App />} />
      <Route path="/collections/:collectionId" element={<App />} />
      <Route path="/documents" element={<App />} />
      <Route path="/doc/:documentId" element={<App />} />
      <Route path="/repos" element={<App />} />
    </Routes>
  </BrowserRouter>,
);
