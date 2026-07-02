import { Field } from "../../ui/Field";
import { Input } from "../../ui/Input";

/** The "File Storage" (work directory) field. */
export function WorkDirInput({
  workDir,
  setWorkDir,
}: {
  workDir: string;
  setWorkDir: (v: string) => void;
}) {
  return (
    <Field label="File Storage" help="Generated files are saved here.">
      <Input
        mono
        value={workDir}
        onChange={(e) => setWorkDir(e.target.value)}
        placeholder="(default: agent data dir)"
      />
    </Field>
  );
}
