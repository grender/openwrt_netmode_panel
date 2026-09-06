// Точка входа новой панели.
//
// На этапе каркаса здесь заглушка: сборка обязана собираться, типы —
// проверяться, а гейт зависимостей — стоять до того, как появится первый
// экран. Иначе первое красное будет означать сразу и «каркас не работает»,
// и «панель написана неправильно».
import { render } from 'preact';
import { ROUTES } from './api/routes.gen';

function App() {
	return <pre>{`netmoded · каркас\n${Object.keys(ROUTES).length} маршрутов из контракта`}</pre>;
}

const root = document.getElementById('app');
if (root) render(<App />, root);
