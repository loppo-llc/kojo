import {
  defaultEffortForModel,
  effortLevelsForModel,
  supportsEffort,
  type EffortLevel,
} from "../../../lib/toolModels";
import { Field } from "../../ui/Field";
import { Select } from "../../ui/Select";

/**
 * The "Effort" field. Rendered only for backends that support an effort
 * selector; the option list + default label are derived from the current
 * model.
 */
export function EffortPicker({
  tool,
  effort,
  setEffort,
  model,
}: {
  tool: string;
  effort: EffortLevel | "";
  setEffort: (e: EffortLevel | "") => void;
  model: string;
}) {
  if (!supportsEffort(tool)) return null;
  return (
    <Field label="Effort">
      <Select
        value={effort}
        onChange={(e) => setEffort(e.target.value as EffortLevel | "")}
      >
        <option value="">default ({defaultEffortForModel(model)})</option>
        {effortLevelsForModel(model).map((e) => (
          <option key={e} value={e}>
            {e}
          </option>
        ))}
      </Select>
    </Field>
  );
}
