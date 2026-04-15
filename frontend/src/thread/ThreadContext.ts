import { createContext } from "react";
import { ThreadService } from "./ThreadService";

export const ThreadContext = createContext<ThreadService | null>(null);
